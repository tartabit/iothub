package iotservice

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/amenzhinsky/iothub/common"
	"github.com/amenzhinsky/iothub/eventhub"
	"github.com/amenzhinsky/iothub/logger"
	"pack.ag/amqp"
)

// ClientOption is a client configuration option.
type ClientOption func(c *Client)

// WithHTTPClient changes default http rest client.
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.http = client
	}
}

// WithLogger sets client logger.
func WithLogger(l logger.Logger) ClientOption {
	return func(c *Client) {
		c.logger = l
	}
}

// WithTLSConfig sets TLS config that's used by REST HTTP and AMQP clients.
func WithTLSConfig(config *tls.Config) ClientOption {
	return func(c *Client) {
		c.tls = config
	}
}

const userAgent = "iothub-golang-sdk/dev"

func ParseConnectionString(cs string) (*common.SharedAccessKey, error) {
	m, err := common.ParseConnectionString(
		cs, "HostName", "SharedAccessKeyName", "SharedAccessKey",
	)
	if err != nil {
		return nil, err
	}
	return common.NewSharedAccessKey(
		m["HostName"], m["SharedAccessKeyName"], m["SharedAccessKey"],
	), nil
}

func NewFromConnectionString(cs string, opts ...ClientOption) (*Client, error) {
	sak, err := ParseConnectionString(cs)
	if err != nil {
		return nil, err
	}
	return New(sak, opts...)
}

// New creates new iothub service client.
func New(sak *common.SharedAccessKey, opts ...ClientOption) (*Client, error) {
	c := &Client{
		sak:    sak,
		done:   make(chan struct{}),
		logger: logger.New(logger.LevelWarn, nil),
	}
	for _, opt := range opts {
		opt(c)
	}
	if c.tls == nil {
		c.tls = &tls.Config{RootCAs: common.RootCAs()}
	}
	if c.http == nil {
		c.http = &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs: common.RootCAs(),
				},
			},
		}
	}
	return c, nil
}

// Client is IoT Hub service client.
type Client struct {
	mu     sync.Mutex
	tls    *tls.Config
	conn   *amqp.Client
	done   chan struct{}
	sak    *common.SharedAccessKey
	logger logger.Logger
	http   *http.Client // REST client

	sendMu   sync.Mutex
	sendLink *amqp.Sender

	// TODO: figure out if it makes sense to cache feedback and file notification receivers
}

// newSession connects to IoT Hub's AMQP broker,
// it's needed for sending C2S events and subscribing to events feedback.
//
// It establishes connection only once, subsequent calls return immediately.
func (c *Client) newSession(ctx context.Context) (*amqp.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return c.conn.NewSession() // already connected
	}
	conn, err := amqp.Dial("amqps://"+c.sak.HostName,
		amqp.ConnTLSConfig(c.tls),
		amqp.ConnProperty("com.microsoft:client-version", userAgent),
	)
	if err != nil {
		return nil, err
	}

	c.logger.Debugf("connected to %s", c.sak.HostName)
	if err = c.putTokenContinuously(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, err
	}

	sess, err := conn.NewSession()
	if err != nil {
		return nil, err
	}
	c.conn = conn
	return sess, nil
}

// putTokenContinuously writes token first time in blocking mode and returns
// maintaining token updates in the background until the client is closed.
func (c *Client) putTokenContinuously(ctx context.Context, conn *amqp.Client) error {
	const (
		tokenUpdateInterval = time.Hour

		// we need to update tokens before they expire to prevent disconnects
		// from azure, without interrupting the message flow
		tokenUpdateSpan = 10 * time.Minute
	)

	sess, err := conn.NewSession()
	if err != nil {
		return err
	}
	defer sess.Close(context.Background())

	if err := c.putToken(ctx, sess, tokenUpdateInterval); err != nil {
		return err
	}

	go func() {
		ticker := time.NewTimer(tokenUpdateInterval - tokenUpdateSpan)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if err := c.putToken(context.Background(), sess, tokenUpdateInterval); err != nil {
					c.logger.Errorf("put token error: %s", err)
					return
				}
				ticker.Reset(tokenUpdateInterval - tokenUpdateSpan)
				c.logger.Debugf("token updated")
			case <-c.done:
				return
			}
		}
	}()
	return nil
}

func (c *Client) putToken(
	ctx context.Context, sess *amqp.Session, lifetime time.Duration,
) error {
	send, err := sess.NewSender(
		amqp.LinkTargetAddress("$cbs"),
	)
	if err != nil {
		return err
	}
	defer send.Close(context.Background())

	recv, err := sess.NewReceiver(
		amqp.LinkSourceAddress("$cbs"),
	)
	if err != nil {
		return err
	}
	defer recv.Close(context.Background())

	sas, err := c.sak.Token(c.sak.HostName, lifetime)
	if err != nil {
		return err
	}
	if err = send.Send(ctx, &amqp.Message{
		Value: sas.String(),
		Properties: &amqp.MessageProperties{
			To:      "$cbs",
			ReplyTo: "cbs",
		},
		ApplicationProperties: map[string]interface{}{
			"operation": "put-token",
			"type":      "servicebus.windows.net:sastoken",
			"name":      c.sak.HostName,
		},
	}); err != nil {
		return err
	}

	msg, err := recv.Receive(ctx)
	if err != nil {
		return err
	}
	if err = msg.Accept(); err != nil {
		return err
	}
	return eventhub.CheckMessageResponse(msg)
}

// connectToEventHub connects to IoT Hub endpoint compatible with Eventhub
// for receiving D2C events, it uses different endpoints and authentication
// mechanisms than newSession.
func (c *Client) connectToEventHub(ctx context.Context) (*eventhub.Client, error) {
	sess, err := c.newSession(ctx)
	if err != nil {
		return nil, err
	}

	// iothub broker should redirect us to an eventhub compatible instance
	// straight after subscribing to events stream, for that we need to connect twice
	defer sess.Close(context.Background())

	recv, err := sess.NewReceiver(
		amqp.LinkSourceAddress("messages/events/"),
	)
	if err != nil {
		return nil, err
	}
	defer recv.Close(context.Background())
	_, err = recv.Receive(ctx)
	if err == nil {
		return nil, errorf("expected redirect error")
	}

	rerr, ok := err.(*amqp.DetachError)
	if !ok || rerr.RemoteError.Condition != amqp.ErrorLinkRedirect {
		return nil, err
	}

	// "amqps://{host}:5671/{consumerGroup}/"
	group := rerr.RemoteError.Info["address"].(string)
	group = group[strings.Index(group, ":5671/")+6 : len(group)-1]

	host := rerr.RemoteError.Info["hostname"].(string)
	c.logger.Debugf("redirected to %s eventhub", host)

	tlsCfg := c.tls.Clone()
	tlsCfg.ServerName = host

	eh, err := eventhub.Dial(host, group,
		eventhub.WithLogger(c.logger),
		eventhub.WithTLSConfig(tlsCfg),
		eventhub.WithSASLPlain(c.sak.SharedAccessKeyName, c.sak.SharedAccessKey),
		eventhub.WithConnOption(amqp.ConnProperty("com.microsoft:client-version", userAgent)),
	)
	if err != nil {
		return nil, err
	}
	return eh, nil
}

// EventHandler handles incoming cloud-to-device events.
type EventHandler func(e *Event) error

// Event is a device-to-cloud message.
type Event struct {
	*common.Message
}

// SubscribeEvents subscribes to D2C events.
//
// Event handler is blocking, handle asynchronous processing on your own.
func (c *Client) SubscribeEvents(ctx context.Context, fn EventHandler) error {
	// a new connection is established for every invocation,
	// this made on purpose because normally an app calls the method once
	eh, err := c.connectToEventHub(ctx)
	if err != nil {
		return err
	}
	defer eh.Close()

	return eh.Subscribe(ctx, func(msg *eventhub.Event) error {
		if err := fn(&Event{FromAMQPMessage(msg.Message)}); err != nil {
			return err
		}
		return msg.Accept()
	},
		eventhub.WithSubscribeSince(time.Now()),
	)
}

// SendOption is a send option.
type SendOption func(msg *common.Message) error

// WithSendMessageID sets message id.
func WithSendMessageID(mid string) SendOption {
	return func(msg *common.Message) error {
		msg.MessageID = mid
		return nil
	}
}

// WithSendCorrelationID sets correlation id.
func WithSendCorrelationID(cid string) SendOption {
	return func(msg *common.Message) error {
		msg.CorrelationID = cid
		return nil
	}
}

// WithSendUserID sets user id.
func WithSendUserID(uid string) SendOption {
	return func(msg *common.Message) error {
		msg.UserID = uid
		return nil
	}
}

// AckType is event feedback acknowledgement type.
type AckType string

const (
	// AckNone no feedback.
	AckNone AckType = "none"

	// AckPositive receive a feedback message if the message was completed.
	AckPositive AckType = "positive"

	// AckNegative receive a feedback message if the message expired
	// (or maximum delivery count was reached) without being completed by the device.
	AckNegative AckType = "negative"

	// AckFull both positive and negative.
	AckFull AckType = "full"
)

// WithSendAck sets message confirmation type.
func WithSendAck(ack AckType) SendOption {
	return func(msg *common.Message) error {
		if ack == "" {
			return nil
		}
		return WithSendProperty("iothub-ack", string(ack))(msg)
	}
}

// WithSendExpiryTime sets message expiration time.
func WithSendExpiryTime(t time.Time) SendOption {
	return func(msg *common.Message) error {
		msg.ExpiryTime = &t
		return nil
	}
}

// WithSendProperty sets a message property.
func WithSendProperty(k, v string) SendOption {
	return func(msg *common.Message) error {
		if msg.Properties == nil {
			msg.Properties = map[string]string{}
		}
		msg.Properties[k] = v
		return nil
	}
}

// WithSendProperties same as `WithSendProperty` but accepts map of keys and values.
func WithSendProperties(m map[string]string) SendOption {
	return func(msg *common.Message) error {
		if msg.Properties == nil {
			msg.Properties = map[string]string{}
		}
		for k, v := range m {
			msg.Properties[k] = v
		}
		return nil
	}
}

// SendEvent sends the given cloud-to-device message and returns its id.
// Panics when event is nil.
func (c *Client) SendEvent(
	ctx context.Context,
	deviceID string,
	payload []byte,
	opts ...SendOption,
) error {
	if deviceID == "" {
		return errorf("device id is empty")
	}
	msg := &common.Message{
		To:      "/devices/" + deviceID + "/messages/devicebound",
		Payload: payload,
	}
	for _, opt := range opts {
		if err := opt(msg); err != nil {
			return err
		}
	}

	send, err := c.getSendLink(ctx)
	if err != nil {
		return err
	}
	return send.Send(ctx, toAMQPMessage(msg))
}

// getSendLink caches sender link between calls to speed up sending events.
func (c *Client) getSendLink(ctx context.Context) (*amqp.Sender, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if c.sendLink != nil {
		return c.sendLink, nil
	}
	sess, err := c.newSession(ctx)
	if err != nil {
		return nil, err
	}
	// since the link is cached it's supposed to be closed along with the client itself

	c.sendLink, err = sess.NewSender(
		amqp.LinkTargetAddress("/messages/devicebound"),
	)
	if err != nil {
		_ = sess.Close(context.Background())
		return nil, err
	}
	return c.sendLink, nil
}

// FeedbackHandler handles message feedback.
type FeedbackHandler func(f *Feedback) error

// SubscribeFeedback subscribes to feedback of messages that ack was requested.
func (c *Client) SubscribeFeedback(ctx context.Context, fn FeedbackHandler) error {
	sess, err := c.newSession(ctx)
	if err != nil {
		return err
	}
	defer sess.Close(context.Background())

	recv, err := sess.NewReceiver(
		amqp.LinkSourceAddress("/messages/serviceBound/feedback"),
	)
	if err != nil {
		return err
	}
	defer recv.Close(context.Background())

	for {
		msg, err := recv.Receive(ctx)
		if err != nil {
			return err
		}
		if len(msg.Data) == 0 {
			c.logger.Warnf("zero length data received")
			continue
		}

		var v []*Feedback
		c.logger.Debugf("feedback received: %s", msg.GetData())
		if err = json.Unmarshal(msg.GetData(), &v); err != nil {
			return err
		}
		for _, f := range v {
			if err := fn(f); err != nil {
				return err
			}
		}
		if err = msg.Accept(); err != nil {
			return err
		}
	}
}

// Feedback is message feedback.
type Feedback struct {
	OriginalMessageID  string    `json:"originalMessageId"`
	Description        string    `json:"description"`
	DeviceGenerationID string    `json:"deviceGenerationId"`
	DeviceID           string    `json:"deviceId"`
	EnqueuedTimeUTC    time.Time `json:"enqueuedTimeUtc"`
	StatusCode         string    `json:"statusCode"`
}

// FileNotification is emitted once a blob file is uploaded to the hub.
//
// TODO: structure is yet to define.
type FileNotification struct {
	*amqp.Message
}

// FileNotificationHandler handles file upload notifications.
type FileNotificationHandler func(event *FileNotification) error

// SubscribeFileNotifications subscribes to file notifications.
//
// The feature has to be enabled in the console.
func (c *Client) SubscribeFileNotifications(
	ctx context.Context,
	fn FileNotificationHandler,
) error {
	sess, err := c.newSession(ctx)
	if err != nil {
		return err
	}
	defer sess.Close(context.Background())

	recv, err := sess.NewReceiver(
		amqp.LinkSourceAddress("/messages/serviceBound/filenotifications"),
	)
	if err != nil {
		return err
	}
	defer recv.Close(context.Background())

	for {
		msg, err := recv.Receive(ctx)
		if err != nil {
			return err
		}
		if len(msg.Data) == 0 {
			c.logger.Warnf("zero length data received")
			continue
		}
		if err := fn(&FileNotification{msg}); err != nil {
			return err
		}
		if err = msg.Accept(); err != nil {
			return err
		}
	}
}

// HostName returns service's hostname.
func (c *Client) HostName() string {
	return c.sak.HostName
}

// DeviceConnectionString builds up a connection string for the given device.
func (c *Client) DeviceConnectionString(device *Device, secondary bool) (string, error) {
	key, err := accessKey(device.Authentication, secondary)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("HostName=%s;DeviceId=%s;SharedAccessKey=%s",
		c.sak.HostName, device.DeviceID, key,
	), nil
}

func (c *Client) ModuleConnectionString(module *Module, secondary bool) (string, error) {
	key, err := accessKey(module.Authentication, secondary)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("HostName=%s;DeviceId=%s;ModuleId=%s;SharedAccessKey=%s",
		c.sak.HostName, module.DeviceID, module.ModuleID, key,
	), nil
}

// DeviceSAS generates a GenerateToken token for the named device.
func (c *Client) DeviceSAS(device *Device, duration time.Duration, secondary bool) (string, error) {
	key, err := accessKey(device.Authentication, secondary)
	if err != nil {
		return "", err
	}
	// TODO: accept resource path
	sas, err := common.NewSharedAccessSignature(c.sak.HostName, "", key, time.Now().Add(duration))
	if err != nil {
		return "", err
	}
	return sas.String(), nil
}

func accessKey(auth *Authentication, secondary bool) (string, error) {
	if auth.Type != AuthSAS {
		return "", errorf("invalid authentication type: %s", auth.Type)
	}
	if secondary {
		return auth.SymmetricKey.SecondaryKey, nil
	}
	return auth.SymmetricKey.PrimaryKey, nil
}

func (c *Client) CallDeviceMethod(
	ctx context.Context,
	deviceID string,
	call *MethodCall,
) (*MethodResult, error) {
	return c.callMethod(
		ctx,
		pathf("twins/%s/methods", deviceID),
		call,
	)
}

func (c *Client) CallModuleMethod(
	ctx context.Context,
	deviceID,
	moduleID string,
	call *MethodCall,
) (*MethodResult, error) {
	return c.callMethod(
		ctx,
		pathf("twins/%s/modules/%s/methods", deviceID, moduleID),
		call,
	)
}

func (c *Client) callMethod(ctx context.Context, path string, call *MethodCall) (
	*MethodResult, error,
) {
	var res MethodResult
	if _, err := c.call(
		ctx,
		http.MethodPost,
		path,
		nil,
		call,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetDevice retrieves the named device.
func (c *Client) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	var res Device
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("devices/%s", deviceID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// CreateDevice creates a new device.
func (c *Client) CreateDevice(ctx context.Context, device *Device) (*Device, error) {
	var res Device
	if _, err := c.call(
		ctx,
		http.MethodPut,
		pathf("devices/%s", device.DeviceID),
		nil,
		device,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// CreateDevices creates array of devices in bulk mode.
func (c *Client) CreateDevices(
	ctx context.Context, devices []*Device,
) (*BulkResult, error) {
	return c.bulkRequest(ctx, devices, "create")
}

// UpdateDevices updates array of devices in bulk mode.
func (c *Client) UpdateDevices(
	ctx context.Context, devices []*Device, force bool,
) (*BulkResult, error) {
	op := "UpdateIfMatchETag"
	if force {
		op = "Update"
	}
	return c.bulkRequest(ctx, devices, op)
}

// DeleteDevices deletes array of devices in bulk mode.
func (c *Client) DeleteDevices(
	ctx context.Context, devices []*Device, force bool,
) (*BulkResult, error) {
	op := "DeleteIfMatchETag"
	if force {
		op = "Delete"
	}
	return c.bulkRequest(ctx, devices, op)
}

func (c *Client) bulkRequest(
	ctx context.Context, devices []*Device, op string,
) (*BulkResult, error) {
	// convert devices into a variable map and rename deviceId to id
	devs := make([]map[string]interface{}, 0, len(devices))
	for _, dev := range devices {
		m, err := toMap(dev)
		if err != nil {
			return nil, err
		}
		id := m["deviceId"]
		delete(m, "deviceId")
		m["id"] = id
		m["importMode"] = op
		devs = append(devs, m)
	}

	var res BulkResult
	_, err := c.call(ctx, http.MethodPost, "devices", nil, devs, &res)
	if err != nil {
		if re, ok := err.(*RequestError); ok && re.Code == http.StatusBadRequest {
			return &res, nil
		}
		return nil, err
	}
	return &res, err
}

// ridiculous way to convert a structure to a variable map
func toMap(v interface{}) (map[string]interface{}, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]interface{}
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func ifMatchHeader(etag string) http.Header {
	if etag == "" {
		etag = "*"
	} else {
		etag = `"` + etag + `"`
	}
	return http.Header{"If-Match": {etag}}
}

// UpdateDevice updates the named device.
func (c *Client) UpdateDevice(ctx context.Context, device *Device) (*Device, error) {
	var res Device
	if _, err := c.call(
		ctx,
		http.MethodPut,
		pathf("devices/%s", device.DeviceID),
		ifMatchHeader(device.ETag),
		device,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// DeleteDevice deletes the named device.
func (c *Client) DeleteDevice(ctx context.Context, device *Device) error {
	_, err := c.call(
		ctx,
		http.MethodDelete,
		pathf("devices/%s", device.DeviceID),
		ifMatchHeader(device.ETag),
		nil,
		nil,
	)
	return err
}

// ListDevices lists all registered devices.
func (c *Client) ListDevices(ctx context.Context) ([]*Device, error) {
	var res []*Device
	if _, err := c.call(
		ctx,
		http.MethodGet,
		"devices",
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

// ListModules list all the registered modules on the named device.
func (c *Client) ListModules(ctx context.Context, deviceID string) ([]*Module, error) {
	var res []*Module
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("devices/%s/modules", deviceID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

// CreateModule adds the given module to the registry.
func (c *Client) CreateModule(ctx context.Context, module *Module) (*Module, error) {
	var res Module
	if _, err := c.call(ctx,
		http.MethodPut,
		pathf("devices/%s/modules/%s", module.DeviceID, module.ModuleID),
		nil,
		module,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetModule retrieves the named module.
func (c *Client) GetModule(ctx context.Context, deviceID, moduleID string) (
	*Module, error,
) {
	var res Module
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("devices/%s/modules/%s", deviceID, moduleID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// UpdateModule updates the given module.
func (c *Client) UpdateModule(ctx context.Context, module *Module) (*Module, error) {
	var res Module
	if _, err := c.call(
		ctx,
		http.MethodPut,
		pathf("devices/%s/modules/%s", module.DeviceID, module.ModuleID),
		ifMatchHeader(module.ETag),
		module,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// DeleteModule removes the named device module.
func (c *Client) DeleteModule(ctx context.Context, module *Module) error {
	_, err := c.call(
		ctx,
		http.MethodDelete,
		pathf("devices/%s/modules/%s", module.DeviceID, module.ModuleID),
		ifMatchHeader(module.ETag),
		nil,
		nil,
	)
	return err
}

// GetDeviceTwin retrieves the named twin device from the registry.
func (c *Client) GetDeviceTwin(ctx context.Context, deviceID string) (*Twin, error) {
	var res Twin
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("twins/%s", deviceID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetModuleTwin retrieves the named module's path.
func (c *Client) GetModuleTwin(ctx context.Context, deviceID, moduleID string) (*ModuleTwin, error) {
	var res ModuleTwin
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("twins/%s/modules/%s", deviceID, moduleID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// UpdateDeviceTwin updates the named twin desired properties.
func (c *Client) UpdateDeviceTwin(ctx context.Context, twin *Twin) (*Twin, error) {
	var res Twin
	if _, err := c.call(
		ctx,
		http.MethodPatch,
		pathf("twins/%s", twin.DeviceID),
		ifMatchHeader(twin.ETag),
		twin,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// UpdateModuleTwin updates the named module twin's desired attributes.
func (c *Client) UpdateModuleTwin(ctx context.Context, twin *ModuleTwin) (
	*ModuleTwin, error,
) {
	var res ModuleTwin
	if _, err := c.call(
		ctx,
		http.MethodPatch,
		pathf("twins/%s/modules/%s", twin.DeviceID, twin.ModuleID),
		ifMatchHeader(twin.ETag),
		twin,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// ListConfigurations gets all available configurations from the registry.
func (c *Client) ListConfigurations(ctx context.Context) ([]*Configuration, error) {
	var res []*Configuration
	if _, err := c.call(
		ctx,
		http.MethodGet,
		"configurations",
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

// CreateConfiguration adds the given configuration to the registry.
func (c *Client) CreateConfiguration(ctx context.Context, config *Configuration) (
	*Configuration, error,
) {
	var res Configuration
	if _, err := c.call(
		ctx,
		http.MethodPut,
		pathf("configurations/%s", config.ID),
		nil,
		config,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// GetConfiguration gets the named configuration from the registry.
func (c *Client) GetConfiguration(ctx context.Context, configID string) (
	*Configuration, error,
) {
	var res Configuration
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("configurations/%s", configID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// UpdateConfiguration updates the given configuration in the registry.
func (c *Client) UpdateConfiguration(ctx context.Context, config *Configuration) (
	*Configuration, error,
) {
	var res Configuration
	if _, err := c.call(
		ctx,
		http.MethodPut,
		pathf("configurations/%s", config.ID),
		ifMatchHeader(config.ETag),
		config,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// DeleteConfiguration removes the given configuration from the registry.
func (c *Client) DeleteConfiguration(ctx context.Context, config *Configuration) error {
	_, err := c.call(
		ctx,
		http.MethodDelete,
		pathf("configurations/%s", config.ID),
		ifMatchHeader(config.ETag),
		nil,
		nil,
	)
	return err
}

func (c *Client) ApplyConfigurationContentOnDevice(
	ctx context.Context,
	deviceID string,
	content *ConfigurationContent,
) error {
	_, err := c.call(
		ctx,
		http.MethodPost,
		pathf("devices/%s/applyConfigurationContent", deviceID),
		nil,
		content,
		nil,
	)
	return err
}

func (c *Client) QueryDevices(
	ctx context.Context, q *Query, fn func(v map[string]interface{}) error,
) error {
	var token string
ReadNext:
	v, token, err := c.execQuery(ctx, q, token)
	if err != nil {
		return err
	}
	for i := range v {
		if err := fn(v[i]); err != nil {
			return err
		}
	}
	if token != "" {
		goto ReadNext
	}
	return nil
}

func (c *Client) execQuery(ctx context.Context, q *Query, token string) (
	[]map[string]interface{}, string, error,
) {
	h := http.Header{}
	if token != "" {
		h.Add("x-ms-continuation", token)
	}
	if q.PageSize > 0 {
		h.Add("x-ms-max-item-count", fmt.Sprintf("%d", q.PageSize))
	}
	var res []map[string]interface{}
	header, err := c.call(
		ctx,
		http.MethodPost,
		"devices/query",
		h,
		q,
		&res,
	)
	if err != nil {
		return nil, "", err
	}
	return res, header.Get("x-ms-continuation"), nil
}

// Stats retrieves the device registry statistic.
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	var res Stats
	if _, err := c.call(
		ctx,
		http.MethodGet,
		"statistics/devices",
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return &res, nil
}

// CreateJob creates import / export jobs.
//
// https://docs.microsoft.com/en-us/azure/iot-hub/iot-hub-bulk-identity-mgmt#get-the-container-sas-uri
func (c *Client) CreateJob(ctx context.Context, job *Job) (map[string]interface{}, error) {
	var res map[string]interface{}
	if _, err := c.call(
		ctx,
		http.MethodPost,
		"jobs/create",
		nil,
		job,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

// ListJobs lists all running jobs.
func (c *Client) ListJobs(ctx context.Context) ([]map[string]interface{}, error) {
	var res []map[string]interface{}
	if _, err := c.call(
		ctx,
		http.MethodGet,
		"jobs",
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) GetJob(ctx context.Context, jobID string) (map[string]interface{}, error) {
	var res map[string]interface{}
	if _, err := c.call(
		ctx,
		http.MethodGet,
		pathf("jobs/%s", jobID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) CancelJob(ctx context.Context, jobID string) (map[string]interface{}, error) {
	var res map[string]interface{}
	if _, err := c.call(
		ctx,
		http.MethodDelete,
		pathf("jobs/%s", jobID),
		nil,
		nil,
		&res,
	); err != nil {
		return nil, err
	}
	return res, nil
}

func (c *Client) call(
	ctx context.Context,
	method, path string,
	headers http.Header,
	r, v interface{}, // request and response objects
) (http.Header, error) {
	var b []byte
	if r != nil {
		var err error
		b, err = json.Marshal(r)
		if err != nil {
			return nil, err
		}
	}

	uri := "https://" + c.sak.HostName + "/" + path + "?api-version=2019-03-30"
	req, err := http.NewRequest(method, uri, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	sas, err := c.sak.Token(c.sak.HostName, time.Hour)
	if err != nil {
		return nil, err
	}

	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", sas.String())
	req.Header.Set("Request-Id", genRequestID())
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		for i := range v {
			req.Header.Add(k, v[i])
		}
	}

	c.logger.Debugf("%s", (*requestOutDump)(req))
	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	c.logger.Debugf("%s", (*responseDump)(res))

	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNoContent {
		return res.Header, nil
	}
	if err = json.Unmarshal(body, v); err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, &RequestError{Code: res.StatusCode, Body: body}
	}
	return res.Header, nil
}

type RequestError struct {
	Code int
	Body []byte
}

func (e *RequestError) Error() string {
	return fmt.Sprintf("code = %d, body = %q", e.Code, e.Body)
}

func genRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// Close closes transport.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
		return nil
	default:
		close(c.done)
	}
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

func pathf(format string, s ...string) string {
	v := make([]interface{}, len(s))
	for i := range s {
		v[i] = url.PathEscape(s[i])
	}
	return fmt.Sprintf(format, v...)
}

func errorf(format string, v ...interface{}) error {
	return fmt.Errorf("iotservice: "+format, v...)
}
