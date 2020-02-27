package mqtt

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/amenzhinsky/iothub/logger"

	"github.com/amenzhinsky/iothub/common"
	"github.com/amenzhinsky/iothub/iotmodule/transport"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// DefaultQoS is the default quality of service value.
const DefaultQoS = 1

// TransportOption is a transport configuration option.
type TransportOption func(tr *Transport)

// WithLogger sets logger for errors and warnings
// plus debug messages when it's enabled.
func WithLogger(l logger.Logger) TransportOption {
	return func(tr *Transport) {
		tr.logger = l
	}
}

// WithClientOptionsConfig configures the mqtt client options structure,
// use it only when you know EXACTLY what you're doing, because changing
// some of opts attributes may lead to unexpected behaviour.
//
// Typical usecase is to change adjust connect or reconnect interval.
func WithClientOptionsConfig(fn func(opts *mqtt.ClientOptions)) TransportOption {
	if fn == nil {
		panic("fn is nil")
	}
	return func(tr *Transport) {
		tr.cocfg = fn
	}
}

// New returns new Transport transport.
// See more: https://docs.microsoft.com/en-us/azure/iot-hub/iot-hub-mqtt-support
func New(opts ...TransportOption) transport.Transport {
	tr := &Transport{
		done: make(chan struct{}),
	}
	for _, opt := range opts {
		opt(tr)
	}
	return tr
}

type Transport struct {
	mu   sync.RWMutex
	conn mqtt.Client

	did         string // device id
	mid         string // module id
	gid         string // generation id
	rid         uint32 // request id, incremented each request
	edgeGateway bool   // connect via edge gateway

	subm sync.RWMutex // cannot use mu for protecting subs
	subs []subFunc    // on-connect mqtt subscriptions

	done chan struct{}         // closed when the transport is closed
	resp map[uint32]chan *resp // responses from iothub

	logger logger.Logger
	cocfg  func(opts *mqtt.ClientOptions)
}

type resp struct {
	code int
	body []byte

	ver int // twin response only
}

func (tr *Transport) SetLogger(logger logger.Logger) {
	tr.logger = logger
}

func (tr *Transport) Connect(ctx context.Context, creds transport.Credentials) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.conn != nil {
		return errors.New("already connected")
	}

	tlsCfg := &tls.Config{}

	if creds.UseEdgeGateway() {
		wluri := os.Getenv("IOTEDGE_WORKLOADURI")
		apiv := os.Getenv("IOTEDGE_APIVERSION")
		tlsCfg.RootCAs = common.TrustBundle(wluri, apiv)
		tlsCfg.InsecureSkipVerify = true // x509: certificate signed by unknown authority if missing
	} else {
		tlsCfg.RootCAs = common.RootCAs()
	}

	if crt := creds.GetCertificate(); crt != nil {
		tlsCfg.Certificates = append(tlsCfg.Certificates, *crt)
	}

	// https://docs.microsoft.com/en-us/azure/iot-hub/iot-hub-mqtt-support#using-the-mqtt-protocol-directly-as-a-module
	//
	// Using the MQTT protocol directly (as a module)
	//
	// Connecting to IoT Hub over MQTT using a module identity is similar to the device (described above) but you need to use the following:
	// Set the client ID to {device_id}/{module_id}.
	// If authenticating with username and password, set the username to <hubname>.azure-devices.net/{device_id}/{module_id}/?api-version=2018-06-30 and use the SAS token associated with the module identity as your password.
	// Use devices/{device_id}/modules/{module_id}/messages/events/ as topic for publishing telemetry.
	// Use devices/{device_id}/modules/{module_id}/messages/events/ as WILL topic.
	// The twin GET and PATCH topics are identical for modules and devices.
	// The twin status topic is identical for modules and devices.

	username := creds.GetHostName() + "/" + creds.GetDeviceID() + "/" + creds.GetModuleID() + "/?api-version=2018-06-30"
	tr.logger.Warnf("mqtt username: %s", username)
	o := mqtt.NewClientOptions()
	o.SetTLSConfig(tlsCfg)
	o.AddBroker("tls://" + creds.GetBroker() + ":8883")
	o.SetClientID(creds.GetDeviceID() + "/" + creds.GetModuleID())
	o.SetCredentialsProvider(func() (string, string) {
		if crt := creds.GetCertificate(); crt != nil {
			return username, ""
		}
		// TODO: renew token only when it expires in case an external token provider is used
		// TODO: this can slow down the reconnect feature, so need to figure out max token lifetime
		audience := url.QueryEscape(creds.GetHostName() + "/devices/" + creds.GetDeviceID() + "/modules/" + creds.GetModuleID())
		sas, err := creds.Token(audience, time.Hour)
		if err != nil {
			tr.logger.Errorf("cannot generate token: %s", err)
			return "", ""
		}
		tr.logger.Warnf("mqtt password: %s", sas.String())
		return username, sas.String()
	})
	o.SetWriteTimeout(30 * time.Second)
	o.SetMaxReconnectInterval(30 * time.Second) // default is 15min, way to long
	o.SetOnConnectHandler(func(c mqtt.Client) {
		tr.logger.Debugf("connection established")
		tr.subm.RLock()
		for _, sub := range tr.subs {
			if err := sub(); err != nil {
				tr.logger.Debugf("on-connect error: %s", err)
			}
		}
		tr.subm.RUnlock()
	})
	o.SetConnectionLostHandler(func(_ mqtt.Client, err error) {
		tr.logger.Debugf("connection lost: %v", err)
	})

	if tr.cocfg != nil {
		tr.cocfg(o)
	}

	c := mqtt.NewClient(o)
	if err := contextToken(ctx, c.Connect()); err != nil {
		return err
	}

	tr.did = creds.GetDeviceID()
	tr.mid = creds.GetModuleID()
	tr.gid = creds.GetGenerationID()
	tr.edgeGateway = creds.UseEdgeGateway()
	tr.conn = c
	return nil
}

type subFunc func() error

// sub invokes the given sub function and if it passes with no error,
// pushes it to the on-re-connect subscriptions list, because the client
// has to resubscribe every reconnect.
func (tr *Transport) sub(sub subFunc) error {
	if err := sub(); err != nil {
		return err
	}
	tr.subm.Lock()
	tr.subs = append(tr.subs, sub)
	tr.subm.Unlock()
	return nil
}

func (tr *Transport) SubscribeEvents(ctx context.Context, mux transport.MessageDispatcher) error {
	return tr.sub(tr.subEvents(ctx, mux))
}

func (tr *Transport) subEvents(ctx context.Context, mux transport.MessageDispatcher) subFunc {
	return func() error {
		return contextToken(ctx, tr.conn.Subscribe(
			"devices/"+tr.did+"/modules/"+tr.mid+"/inputs/#", DefaultQoS, func(_ mqtt.Client, m mqtt.Message) {
				msg, err := parseEventMessage(m)
				if err != nil {
					tr.logger.Errorf("message parse error: %s", err)
					return
				}
				mux.Dispatch(msg)
			},
		))
	}
}

func (tr *Transport) SubscribeTwinUpdates(ctx context.Context, mux transport.TwinStateDispatcher) error {
	return tr.sub(tr.subTwinUpdates(ctx, mux))
}

func (tr *Transport) subTwinUpdates(ctx context.Context, mux transport.TwinStateDispatcher) subFunc {
	return func() error {
		return contextToken(ctx, tr.conn.Subscribe(
			"$iothub/twin/PATCH/properties/desired/#", DefaultQoS, func(_ mqtt.Client, m mqtt.Message) {
				mux.Dispatch(m.Payload())
			},
		))
	}
}

func parseEventMessage(m mqtt.Message) (*common.Message, error) {
	p, err := parseCloudToDeviceTopic(m.Topic())
	if err != nil {
		return nil, err
	}
	e := &common.Message{
		Payload:    m.Payload(),
		Properties: make(map[string]string, len(p)),
	}
	for k, v := range p {
		switch k {
		case "$.mid":
			e.MessageID = v
		case "$.cid":
			e.CorrelationID = v
		case "$.uid":
			e.UserID = v
		case "$.to":
			e.To = v
		case "$.exp":
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return nil, err
			}
			e.ExpiryTime = &t
		default:
			e.Properties[k] = v
		}
	}
	return e, nil
}

// devices/{device}/messages/devicebound/%24.to=%2Fdevices%2F{device}%2Fmessages%2FdeviceBound&a=b&b=c
func parseCloudToDeviceTopic(s string) (map[string]string, error) {
	s, err := url.QueryUnescape(s)
	if err != nil {
		return nil, err
	}

	// attributes prefixed with $.,
	// e.g. `messageId` becomes `$.mid`, `to` becomes `$.to`, etc.
	i := strings.Index(s, "$.")
	if i == -1 {
		return nil, errors.New("malformed cloud-to-device topic name")
	}
	q, err := url.ParseQuery(s[i:])
	if err != nil {
		return nil, err
	}

	p := make(map[string]string, len(q))
	for k, v := range q {
		if len(v) != 1 {
			return nil, fmt.Errorf("unexpected number of property values: %d", len(q))
		}
		p[k] = v[0]
	}
	return p, nil
}

func (tr *Transport) RegisterDirectMethods(ctx context.Context, mux transport.MethodDispatcher) error {
	return tr.sub(tr.subDirectMethods(ctx, mux))
}

func (tr *Transport) subDirectMethods(ctx context.Context, mux transport.MethodDispatcher) subFunc {
	return func() error {
		return contextToken(ctx, tr.conn.Subscribe(
			"$iothub/methods/POST/#", DefaultQoS, func(_ mqtt.Client, m mqtt.Message) {
				method, rid, err := parseDirectMethodTopic(m.Topic())
				if err != nil {
					tr.logger.Errorf("parse error: %s", err)
					return
				}
				rc, b, err := mux.Dispatch(method, m.Payload())
				if err != nil {
					tr.logger.Errorf("dispatch error: %s", err)
					return
				}
				dst := fmt.Sprintf("$iothub/methods/res/%d/?$rid=%d", rc, rid)
				if err = tr.send(ctx, dst, DefaultQoS, b); err != nil {
					tr.logger.Errorf("method response error: %s", err)
					return
				}
			},
		))
	}
}

// returns method name and rid
// format: $iothub/methods/POST/{method}/?$rid={rid}
func parseDirectMethodTopic(s string) (string, int, error) {
	const prefix = "$iothub/methods/POST/"

	s, err := url.QueryUnescape(s)
	if err != nil {
		return "", 0, err
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", 0, err
	}

	p := strings.TrimRight(u.Path, "/")
	if !strings.HasPrefix(p, prefix) {
		return "", 0, errors.New("malformed direct method topic")
	}

	q := u.Query()
	if len(q["$rid"]) != 1 {
		return "", 0, errors.New("$rid is not available")
	}
	rid, err := strconv.Atoi(q["$rid"][0])
	if err != nil {
		return "", 0, fmt.Errorf("$rid parse error: %s", err)
	}
	return p[len(prefix):], rid, nil
}

func (tr *Transport) RetrieveTwinProperties(ctx context.Context) ([]byte, error) {
	r, err := tr.request(ctx, "$iothub/twin/GET/?$rid=%d", nil)
	if err != nil {
		return nil, err
	}
	return r.body, nil
}

func (tr *Transport) UpdateTwinProperties(ctx context.Context, b []byte) (int, error) {
	r, err := tr.request(ctx, "$iothub/twin/PATCH/properties/reported/?$rid=%d", b)
	if err != nil {
		return 0, err
	}
	return r.ver, nil
}

func (tr *Transport) request(ctx context.Context, topic string, b []byte) (*resp, error) {
	if err := tr.enableTwinResponses(ctx); err != nil {
		return nil, err
	}
	rid := atomic.AddUint32(&tr.rid, 1) // increment rid counter
	dst := fmt.Sprintf(topic, rid)
	rch := make(chan *resp, 1)
	tr.mu.Lock()
	tr.resp[rid] = rch
	tr.mu.Unlock()
	defer func() {
		tr.mu.Lock()
		delete(tr.resp, rid)
		tr.mu.Unlock()
	}()

	if err := tr.send(ctx, dst, DefaultQoS, b); err != nil {
		return nil, err
	}

	select {
	case r := <-rch:
		if r.code < 200 && r.code > 299 {
			return nil, fmt.Errorf("request failed with %d response code", r.code)
		}
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (tr *Transport) enableTwinResponses(ctx context.Context) error {
	tr.mu.Lock()
	defer tr.mu.Unlock()

	// already subscribed
	if tr.resp != nil {
		return nil
	}
	if err := tr.sub(tr.subTwinResponses(ctx)); err != nil {
		return err
	}
	tr.resp = make(map[uint32]chan *resp)
	return nil
}

func (tr *Transport) subTwinResponses(ctx context.Context) subFunc {
	return func() error {
		return contextToken(ctx, tr.conn.Subscribe(
			"$iothub/twin/res/#", DefaultQoS, func(_ mqtt.Client, m mqtt.Message) {
				rc, rid, ver, err := parseTwinPropsTopic(m.Topic())
				if err != nil {
					fmt.Printf("parse twin props topic error: %s", err)
					return
				}

				tr.mu.RLock()
				defer tr.mu.RUnlock()
				for r, rch := range tr.resp {
					if int(r) != rid {
						continue
					}
					res := &resp{code: rc, ver: ver, body: m.Payload()}
					select {
					case rch <- res:
						// try to push without a goroutine first
						// if the channel buffer is not busy
					default:
						go func() {
							rch <- res
						}()
					}
					return
				}
				tr.logger.Warnf("unknown rid: %q", rid)
			},
		))
	}
}

// parseTwinPropsTopic parses the given topic name into rc, rid and ver.
// $iothub/twin/res/{rc}/?$rid={rid}(&$version={ver})?
func parseTwinPropsTopic(s string) (int, int, int, error) {
	const prefix = "$iothub/twin/res/"

	u, err := url.Parse(s)
	if err != nil {
		return 0, 0, 0, err
	}

	p := strings.Trim(u.Path, "/")
	if !strings.HasPrefix(p, prefix) {
		return 0, 0, 0, errors.New("malformed twin response topic")
	}
	rc, err := strconv.Atoi(p[len(prefix):])
	if err != nil {
		return 0, 0, 0, err
	}

	q := u.Query()
	if len(q["$rid"]) != 1 {
		return 0, 0, 0, errors.New("$rid is not available")
	}
	rid, err := strconv.Atoi(q["$rid"][0])
	if err != nil {
		return 0, 0, 0, fmt.Errorf("$rid parse error: %s", err)
	}

	var ver int // version is available only for update responses
	if len(q["$version"]) == 1 {
		ver, err = strconv.Atoi(q["$version"][0])
		if err != nil {
			return 0, 0, 0, err
		}
	}
	return rc, rid, ver, nil
}

func (tr *Transport) Send(ctx context.Context, msg *common.Message) error {
	// this is just copying functionality from the nodejs sdk, but
	// seems like adding meta attributes does nothing or in some cases,
	// e.g. when $.exp is set the cloud just disconnects.
	u := make(url.Values, len(msg.Properties)+5)
	if msg.MessageID != "" {
		u["$.mid"] = []string{msg.MessageID}
	}
	if msg.CorrelationID != "" {
		u["$.cid"] = []string{msg.CorrelationID}
	}
	if msg.UserID != "" {
		u["$.uid"] = []string{msg.UserID}
	}
	if msg.To != "" {
		u["$.to"] = []string{msg.To}
	}
	if msg.ExpiryTime != nil && !msg.ExpiryTime.IsZero() {
		u["$.exp"] = []string{msg.ExpiryTime.UTC().Format(time.RFC3339)}
	}
	for k, v := range msg.Properties {
		u[k] = []string{v}
	}

	dst := "devices/" + tr.did + "/modules/" + tr.mid + "/messages/events/" + u.Encode()
	tr.logger.Warnf("send message destination: %s", dst)
	qos := DefaultQoS
	if q, ok := msg.TransportOptions["qos"]; ok {
		qos = q.(int) // panic if it's not an int
		if qos != 0 && qos != 1 {
			return fmt.Errorf("invalid QoS value: %d", qos)
		}
	}
	return tr.send(ctx, dst, qos, msg.Payload)
}

func (tr *Transport) send(ctx context.Context, topic string, qos int, b []byte) error {
	tr.mu.RLock()
	if tr.conn == nil {
		tr.mu.RUnlock()
		return errors.New("not connected")
	}
	tr.mu.RUnlock()
	return contextToken(ctx, tr.conn.Publish(topic, byte(qos), false, b))
}

// mqtt lib doesn't support contexts currently
func contextToken(ctx context.Context, t mqtt.Token) error {
	done := make(chan struct{})
	go func() {
		for !t.WaitTimeout(time.Second) {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
		close(done)
	}()
	select {
	case <-done:
		return t.Error()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (tr *Transport) Close() error {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	select {
	case <-tr.done:
		return nil
	default:
		close(tr.done)
	}
	if tr.conn != nil && tr.conn.IsConnected() {
		tr.conn.Disconnect(250)
		tr.logger.Debugf("disconnected")
	}
	return nil
}
