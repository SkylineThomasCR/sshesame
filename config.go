package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"

	"github.com/adrg/xdg"
	"github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v2"
)

type config struct {
	ListenAddress           string
	RekeyThreshold          uint64
	KeyExchanges            []string
	Ciphers                 []string
	MACs                    []string
	HostKeys                []string
	NoClientAuth            bool
	MaxAuthTries            int
	PasswordAuth            struct{ Enabled, Accepted bool }
	PublicKeyAuth           struct{ Enabled, Accepted bool }
	KeyboardInteractiveAuth struct {
		Enabled, Accepted bool
		Instruction       string
		Questions         []struct {
			Text string
			Echo bool
		}
	}
	ServerVersion string
	Banner        string
}

func (cfg config) createSSHServerConfig() *ssh.ServerConfig {
	sshServerConfig := &ssh.ServerConfig{
		Config: ssh.Config{
			RekeyThreshold: cfg.RekeyThreshold,
			KeyExchanges:   cfg.KeyExchanges,
			Ciphers:        cfg.Ciphers,
			MACs:           cfg.MACs,
		},
		NoClientAuth: cfg.NoClientAuth,
		MaxAuthTries: cfg.MaxAuthTries,
		AuthLogCallback: func(conn ssh.ConnMetadata, method string, err error) {
			getLogEntry(conn).WithFields(logrus.Fields{
				"method":  method,
				"success": err == nil,
			}).Infoln("Client attempted to authenticate")
		},
		ServerVersion:  cfg.ServerVersion,
		BannerCallback: func(conn ssh.ConnMetadata) string { return strings.ReplaceAll(cfg.Banner, "\n", "\r\n") },
	}
	if cfg.PasswordAuth.Enabled {
		sshServerConfig.PasswordCallback = func(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
			getLogEntry(conn).WithFields(logrus.Fields{
				"password": string(password),
				"success":  cfg.PasswordAuth.Accepted,
			}).Infoln("Password authentication attempted")
			if !cfg.PasswordAuth.Accepted {
				return nil, errors.New("")
			}
			return nil, nil
		}
	}
	if cfg.PublicKeyAuth.Enabled {
		sshServerConfig.PublicKeyCallback = func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			getLogEntry(conn).WithFields(logrus.Fields{
				"public_key_fingerprint": ssh.FingerprintSHA256(key),
				"success":                cfg.PublicKeyAuth.Accepted,
			}).Infoln("Public key authentication attempted")
			if !cfg.PublicKeyAuth.Accepted {
				return nil, errors.New("")
			}
			return nil, nil
		}
	}
	if cfg.KeyboardInteractiveAuth.Enabled {
		sshServerConfig.KeyboardInteractiveCallback = func(conn ssh.ConnMetadata, client ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			var questions []string
			var echos []bool
			for _, question := range cfg.KeyboardInteractiveAuth.Questions {
				questions = append(questions, question.Text)
				echos = append(echos, question.Echo)
			}
			answers, err := client(conn.User(), cfg.KeyboardInteractiveAuth.Instruction, questions, echos)
			if err != nil {
				log.Println("Failed to process keyboard interactive authentication:", err)
				return nil, errors.New("")
			}
			getLogEntry(conn).WithFields(logrus.Fields{
				"answers": strings.Join(answers, ", "),
				"success": cfg.KeyboardInteractiveAuth.Accepted,
			}).Infoln("Keyboard interactive authentication attempted")
			if !cfg.KeyboardInteractiveAuth.Accepted {
				return nil, errors.New("")
			}
			return nil, nil
		}
	}
	for _, hostKeyFileName := range cfg.HostKeys {
		hostKeyBytes, err := ioutil.ReadFile(hostKeyFileName)
		if err != nil {
			log.Fatalln("Failed to read host key", hostKeyFileName, ":", err)
		}
		signer, err := ssh.ParsePrivateKey(hostKeyBytes)
		if err != nil {
			log.Fatalln("Failed to parse host key", hostKeyFileName, ":", err)
		}
		sshServerConfig.AddHostKey(signer)
	}
	return sshServerConfig
}

type hostKeyType int

const (
	rsa_key hostKeyType = iota
	ecdsa_key
	ed25519_key
)

func generateKey(fileName string, keyType hostKeyType) error {
	if _, err := os.Stat(fileName); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		log.Println("Host key", fileName, "not found, generating it")
		if _, err := os.Stat(path.Dir(fileName)); os.IsNotExist(err) {
			if err := os.MkdirAll(path.Dir(fileName), 0755); err != nil {
				return err
			}
		}
		var key interface{}
		switch keyType {
		case rsa_key:
			key, err = rsa.GenerateKey(rand.Reader, 3072)
		case ecdsa_key:
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		case ed25519_key:
			_, key, err = ed25519.GenerateKey(rand.Reader)
		default:
			err = errors.New("unsupported key type")
		}
		if err != nil {
			return err
		}
		keyBytes, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		if err := ioutil.WriteFile(fileName, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
			return err
		}
	}
	return nil
}

func getConfig(fileName string) (*config, error) {
	result := &config{
		ListenAddress: "127.0.0.1:2022",
		ServerVersion: "SSH-2.0-sshesame",
		Banner:        "This is an SSH honeypot. Everything is logged and monitored.",
	}
	result.PasswordAuth.Enabled = true
	result.PasswordAuth.Accepted = true
	result.PublicKeyAuth.Enabled = true
	result.PublicKeyAuth.Accepted = false

	var configBytes []byte
	var err error
	if fileName == "" {
		configBytes, err = ioutil.ReadFile(path.Join(xdg.ConfigHome, "sshesame.yaml"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else {
		configBytes, err = ioutil.ReadFile(fileName)
		if err != nil {
			return nil, err
		}
	}
	if configBytes != nil {
		if err := yaml.UnmarshalStrict(configBytes, result); err != nil {
			return nil, err
		}
	}

	if len(result.HostKeys) == 0 {
		dataDir := path.Join(xdg.DataHome, "sshesame")
		log.Println("No host keys configured, using keys at", dataDir)

		for _, key := range []struct {
			keyType  hostKeyType
			filename string
		}{
			{keyType: rsa_key, filename: "host_rsa_key"},
			{keyType: ecdsa_key, filename: "host_ecdsa_key"},
			{keyType: ed25519_key, filename: "host_ed25519_key"},
		} {
			keyFileName := path.Join(dataDir, key.filename)
			if err := generateKey(keyFileName, key.keyType); err != nil {
				return nil, err
			}
			result.HostKeys = []string{keyFileName}
		}
	}

	return result, nil
}
