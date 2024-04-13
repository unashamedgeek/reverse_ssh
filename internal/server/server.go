package server

import (
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/internal/server/data"
	"github.com/NHAS/reverse_ssh/internal/server/multiplexer"
	"github.com/NHAS/reverse_ssh/internal/server/webhooks"
	"github.com/NHAS/reverse_ssh/internal/server/webserver"
	"github.com/NHAS/reverse_ssh/pkg/mux"
	"golang.org/x/crypto/ssh"
)

func CreateOrLoadServerKeys(privateKeyPath string) (ssh.Signer, error) {

	//If we have already created a private key (or there is one in the current directory) dont overwrite/create another one
	if _, err := os.Stat(privateKeyPath); os.IsNotExist(err) {

		privateKeyPem, err := internal.GeneratePrivateKey()
		if err != nil {
			return nil, fmt.Errorf("unable to generate private key, and no private key specified: %s", err)
		}

		err = os.WriteFile(privateKeyPath, privateKeyPem, 0600)
		if err != nil {
			return nil, fmt.Errorf("unable to write private key to disk: %s", err)
		}
	}

	privateBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("failed to load private key (%s): %s", privateKeyPath, err)
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to parse private key: %s", err)
	}

	return private, nil
}

func Run(addr, dataDir, connectBackAddress, TLSCertPath, TLSKeyPath string, insecure, enabledWebserver, enabletTLS, openproxy bool, timeout int) {
	c := mux.MultiplexerConfig{
		Control:           true,
		Downloads:         enabledWebserver,
		TLS:               enabletTLS,
		TLSCertPath:       TLSCertPath,
		TLSKeyPath:        TLSKeyPath,
		AutoTLSCommonName: connectBackAddress,
		TcpKeepAlive:      timeout,
		PollingAuthChecker: func(key string, addr net.Addr) bool {

			authorizedKey, err := hex.DecodeString(key)
			if err != nil {
				return false
			}

			pubKey, err := ssh.ParsePublicKey(authorizedKey)
			if err != nil {
				return false
			}

			_, err = CheckAuth(filepath.Join(dataDir, "authorized_controllee_keys"), pubKey, getIP(addr.String()), insecure)
			return err == nil

		},
	}

	privateKeyPath := filepath.Join(dataDir, "id_ed25519")

	log.Println("Version: ", internal.Version)
	var err error
	multiplexer.ServerMultiplexer, err = mux.ListenWithConfig("tcp", addr, c)
	if err != nil {
		log.Fatalf("Failed to listen on %s (%s)", addr, err)
	}
	defer multiplexer.ServerMultiplexer.Close()

	log.Printf("Listening on %s\n", addr)

	private, err := CreateOrLoadServerKeys(privateKeyPath)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Loading private key from: %s\n", privateKeyPath)

	log.Println("Server key fingerprint: ", internal.FingerprintSHA256Hex(private.PublicKey()))

	if enabledWebserver {
		if len(connectBackAddress) == 0 {
			connectBackAddress = addr
		}
		go webserver.Start(multiplexer.ServerMultiplexer.DownloadRequests(), connectBackAddress, "../", dataDir, private.PublicKey())

	}

	err = data.LoadDatabase(filepath.Join(dataDir, "data.db"))
	if err != nil {
		log.Fatal(err)
	}

	go webhooks.StartWebhooks()

	StartSSHServer(multiplexer.ServerMultiplexer.ControlRequests(), private, insecure, openproxy, dataDir, timeout)
}
