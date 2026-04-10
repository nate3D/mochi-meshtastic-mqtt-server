package main

import (
	"crypto/tls"
	"encoding/base64"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	mqtt "github.com/mochi-mqtt/server/v2"
	meshhook "github.com/mochi-mqtt/server/v2/hooks/meshtastic"
	"github.com/mochi-mqtt/server/v2/listeners"
	"gopkg.in/yaml.v3"
)

// serverConfig is the top-level configuration file structure.
type serverConfig struct {
	// TCP listener address (plain MQTT). Default ":1883". Set empty to disable.
	TCPAddr string `yaml:"tcp_addr"`
	// TLSAddr is the address for TLS-encrypted MQTT (MQTTS). Default: disabled.
	TLSAddr string `yaml:"tls_addr"`
	// WSAddr is the address for WebSocket MQTT. Default: disabled.
	WSAddr string `yaml:"ws_addr"`
	// CertFile is the path to the PEM TLS certificate file.
	CertFile string `yaml:"cert_file"`
	// KeyFile is the path to the PEM TLS private key file.
	KeyFile string `yaml:"key_file"`
	// Meshtastic contains the Meshtastic hook configuration.
	Meshtastic meshtasticConfig `yaml:"meshtastic"`
}

// meshtasticConfig is the YAML representation of meshhook.Config with
// base64-encoded PSKs for human-readable config files.
type meshtasticConfig struct {
	Credentials        []credentialConfig `yaml:"credentials"`
	Channels           []channelConfig    `yaml:"channels"`
	BlockedPortNums    []int32            `yaml:"blocked_port_nums"`
	AllowedPortNums    []int32            `yaml:"allowed_port_nums"`
	RateLimits         meshhook.RateLimitConfig `yaml:"rate_limits"`
	RequireDecryptable bool               `yaml:"require_decryptable"`
	AllowJSON          bool               `yaml:"allow_json"`
	AllowedRegions     []string           `yaml:"allowed_regions"`
}

// credentialConfig is a username + bcrypt password hash from the config file.
type credentialConfig struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

// channelConfig represents a channel with a base64-encoded PSK for config files.
type channelConfig struct {
	Name   string `yaml:"name"`
	PSKB64 string `yaml:"psk_base64"` // base64-encoded raw PSK bytes
}

func main() {
	configPath := flag.String("config", "config.yaml", "path to server config file")
	flag.Parse()

	logLevel := slog.LevelDebug
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "INFO":
		logLevel = slog.LevelInfo
	case "WARN":
		logLevel = slog.LevelWarn
	case "ERROR":
		logLevel = slog.LevelError
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))

	cfg, err := loadConfig(*configPath, log)
	if err != nil {
		log.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	if cfg.TCPAddr == "" && cfg.TLSAddr == "" && cfg.WSAddr == "" {
		cfg.TCPAddr = ":1883"
	}

	server := mqtt.New(&mqtt.Options{
		Logger: log,
	})

	// Build the hook config, decoding base64 PSKs.
	hookCfg := meshhook.Config{
		BlockedPortNums:    cfg.Meshtastic.BlockedPortNums,
		AllowedPortNums:    cfg.Meshtastic.AllowedPortNums,
		RateLimits:         cfg.Meshtastic.RateLimits,
		RequireDecryptable: cfg.Meshtastic.RequireDecryptable,
		AllowJSON:          cfg.Meshtastic.AllowJSON,
		AllowedRegions:     cfg.Meshtastic.AllowedRegions,
	}

	for _, c := range cfg.Meshtastic.Credentials {
		hookCfg.Credentials = append(hookCfg.Credentials, meshhook.Credential{
			Username:     c.Username,
			PasswordHash: c.PasswordHash,
		})
	}

	// Credentials can also be supplied via env vars BROKER_USERNAME / BROKER_PASSWORD_HASH,
	// which takes precedence and avoids putting secrets in config.yaml.
	if u, h := strings.TrimSpace(os.Getenv("BROKER_USERNAME")), strings.TrimSpace(os.Getenv("BROKER_PASSWORD_HASH")); u != "" && h != "" {
		hookCfg.Credentials = append(hookCfg.Credentials, meshhook.Credential{
			Username:     u,
			PasswordHash: h,
		})
	}

	for _, ch := range cfg.Meshtastic.Channels {
		psk, err := base64.StdEncoding.DecodeString(ch.PSKB64)
		if err != nil {
			log.Error("invalid PSK base64 for channel", "channel", ch.Name, "error", err)
			os.Exit(1)
		}
		hookCfg.Channels = append(hookCfg.Channels, meshhook.ChannelConfig{
			Name: ch.Name,
			PSK:  psk,
		})
	}

	if err := server.AddHook(&meshhook.Hook{}, &hookCfg); err != nil {
		log.Error("failed to add meshtastic hook", "error", err)
		os.Exit(1)
	}

	// Plain TCP listener.
	if cfg.TCPAddr != "" {
		if err := server.AddListener(listeners.NewTCP(listeners.Config{
			ID:      "tcp",
			Address: cfg.TCPAddr,
		})); err != nil {
			log.Error("failed to add TCP listener", "address", cfg.TCPAddr, "error", err)
			os.Exit(1)
		}
	}

	// TLS listener.
	if cfg.TLSAddr != "" {
		if cfg.CertFile == "" || cfg.KeyFile == "" {
			log.Error("tls_addr set but cert_file/key_file not configured")
			os.Exit(1)
		}
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			log.Error("failed to load TLS certificate", "error", err)
			os.Exit(1)
		}
		tlsCfg := &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		}
		if err := server.AddListener(listeners.NewTCP(listeners.Config{
			ID:        "tls",
			Address:   cfg.TLSAddr,
			TLSConfig: tlsCfg,
		})); err != nil {
			log.Error("failed to add TLS listener", "address", cfg.TLSAddr, "error", err)
			os.Exit(1)
		}
	}

	// WebSocket listener.
	if cfg.WSAddr != "" {
		if err := server.AddListener(listeners.NewWebsocket(listeners.Config{
			ID:      "ws",
			Address: cfg.WSAddr,
		})); err != nil {
			log.Error("failed to add WebSocket listener", "address", cfg.WSAddr, "error", err)
			os.Exit(1)
		}
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		log.Info("shutting down")
		if err := server.Close(); err != nil {
			log.Error("error during shutdown", "error", err)
		}
	}()

	if err := server.Serve(); err != nil {
		log.Error("server error", "error", err)
		os.Exit(1)
	}

	// Block until Close() signals the done channel.
	select {}
}

// loadConfig reads and parses the YAML config file. If the file does not exist
// a default config is returned so the server can start with sane defaults.
func loadConfig(path string, log *slog.Logger) (serverConfig, error) {
	cfg := serverConfig{
		TCPAddr: ":1883",
		Meshtastic: meshtasticConfig{
			RateLimits: meshhook.RateLimitConfig{
				PacketsPerWindow: 100,
				WindowSecs:       60,
				DedupWindowSecs:  60,
			},
		},
	}

	data, err := os.ReadFile(path) // #nosec G304 — path is a user-supplied CLI flag
	if err != nil {
		if os.IsNotExist(err) {
			log.Info("config file not found, using defaults", "path", path)
			return cfg, nil
		}
		return cfg, err
	}

	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}

	return cfg, nil
}
