package meshtastic

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
)

const (
	defaultUpstreamBroker  = "mqtt.meshtastic.org:1883"
	upstreamKeepAlive      = 30 * time.Second
	upstreamReconnectDelay = 5 * time.Second
	upstreamPublishTimeout = 5 * time.Second
	upstreamPublishQoS     = byte(0)
)

// Forwarder maintains a persistent MQTT connection to an upstream broker and
// publishes validated Meshtastic packets to it using the same topic path they
// arrived on. Auto-reconnect is handled by the paho client.
type Forwarder struct {
	cfg      UpstreamForwardConfig
	client   paho.Client
	channels map[string]struct{} // empty = forward all channels
	log      *slog.Logger
}

// newForwarder creates a Forwarder and initiates the upstream connection.
// If the initial connect fails, the paho client will continue retrying in the
// background — callers should not treat this as a fatal error.
func newForwarder(cfg UpstreamForwardConfig, log *slog.Logger) *Forwarder {
	f := &Forwarder{
		cfg:      cfg,
		log:      log,
		channels: make(map[string]struct{}, len(cfg.Channels)),
	}
	for _, ch := range cfg.Channels {
		f.channels[ch] = struct{}{}
	}

	broker := cfg.BrokerAddr
	if broker == "" {
		broker = defaultUpstreamBroker
	}

	scheme := "tcp"
	if cfg.TLS {
		scheme = "ssl"
	}
	brokerURL := fmt.Sprintf("%s://%s", scheme, broker)

	clientID := cfg.ClientID
	if clientID == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "mochi"
		}
		clientID = "mochi-meshtastic-" + hostname
	}

	opts := paho.NewClientOptions()
	opts.AddBroker(brokerURL)
	opts.SetClientID(clientID)
	if cfg.Username != "" {
		opts.SetUsername(cfg.Username)
		opts.SetPassword(cfg.Password)
	}
	opts.SetAutoReconnect(true)
	opts.SetCleanSession(true)
	opts.SetKeepAlive(upstreamKeepAlive)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(upstreamReconnectDelay)
	if cfg.TLS {
		opts.SetTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	opts.SetOnConnectHandler(func(_ paho.Client) {
		log.Info("upstream forwarder: connected", "broker", brokerURL)
	})
	opts.SetConnectionLostHandler(func(_ paho.Client, err error) {
		log.Warn("upstream forwarder: connection lost, reconnecting", "broker", brokerURL, "error", err)
	})

	f.client = paho.NewClient(opts)

	tok := f.client.Connect()
	if tok.WaitTimeout(10 * time.Second) {
		if err := tok.Error(); err != nil {
			log.Warn("upstream forwarder: initial connect failed, will retry",
				"broker", brokerURL, "error", err)
		}
	} else {
		log.Warn("upstream forwarder: initial connect timed out, will retry", "broker", brokerURL)
	}

	return f
}

// Forward publishes payload to topic on the upstream broker.
// If a channel allowlist is configured, the packet is only forwarded when
// channel is in the list. Errors are logged and do not propagate — the local
// broker pipeline is never blocked by upstream availability.
func (f *Forwarder) Forward(topic, channel string, payload []byte) {
	if len(f.channels) > 0 {
		if _, ok := f.channels[channel]; !ok {
			return
		}
	}

	if !f.client.IsConnected() {
		f.log.Debug("upstream forwarder: not connected, dropping packet", "topic", topic)
		return
	}

	// QoS 0: fire-and-forget. WaitTimeout flushes the send but does not block
	// on broker acknowledgement (none is expected for QoS 0).
	tok := f.client.Publish(topic, upstreamPublishQoS, false, payload)
	if tok.WaitTimeout(upstreamPublishTimeout) {
		if err := tok.Error(); err != nil {
			f.log.Warn("upstream forwarder: publish failed", "topic", topic, "error", err)
		} else {
			f.log.Debug("upstream forwarder: forwarded packet", "topic", topic, "bytes", len(payload))
		}
	}
}

// Close disconnects from the upstream broker cleanly.
func (f *Forwarder) Close() {
	if f.client != nil {
		f.client.Disconnect(500)
	}
}
