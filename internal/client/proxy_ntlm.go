package client

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/Azure/go-ntlmssp"
)

const NTLM = "NTLM "

var ntlmProxyCreds string

func SetNTLMProxyCreds(creds string) {
	ntlmProxyCreds = creds
}

func ParseNTLMCreds(creds string) (domain, user, pass string, err error) {
	if creds == "" {
		return "", "", "", fmt.Errorf("NTLM credentials not provided. Use --ntlm-proxy-creds in format DOMAIN\\USER:PASS")
	}

	parts := strings.Split(creds, "\\")
	if len(parts) != 2 {
		return "", "", "", fmt.Errorf("invalid NTLM credentials format. Expected DOMAIN\\USER:PASS, got %q", creds)
	}

	domain = parts[0]
	// Find the first colon after the domain\user portion
	userPassParts := strings.SplitN(parts[1], ":", 2)
	if len(userPassParts) != 2 {
		return "", "", "", fmt.Errorf("invalid NTLM credentials format. Expected DOMAIN\\USER:PASS, got %q", creds)
	}

	return domain, userPassParts[0], userPassParts[1], nil
}

func getNTLMAuthHeader(_ string, challengeResponse []byte) (string, error) {
	domain, user, pass, err := ParseNTLMCreds(ntlmProxyCreds)
	if err != nil {
		return "", err
	}

	if len(challengeResponse) == 0 {
		// Type 1 message - Initial Negotiate
		negotiateMessage, err := ntlmssp.NewNegotiateMessage(domain, "")
		if err != nil {
			return "", fmt.Errorf("failed to create NTLM negotiate message: %v", err)
		}
		return NTLM + base64.StdEncoding.EncodeToString(negotiateMessage), nil
	}

	// Type 3 message - Authentication
	authenticateMessage, err := ntlmssp.ProcessChallenge(challengeResponse, user, pass, true)
	if err != nil {
		return "", fmt.Errorf("failed to process NTLM challenge: %v", err)
	}
	return NTLM + base64.StdEncoding.EncodeToString(authenticateMessage), nil
}
