package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// pairing holds the response shapes the pair flow needs.
type pairingStart struct {
	PairingID string `json:"pairingId"`
	Code      string `json:"code"`
	ExpiresAt string `json:"expiresAt"`
}

type pairingPoll struct {
	Status string `json:"status"`
	Config *struct {
		ServerURL      string `json:"serverUrl"`
		ConnectorID    string `json:"connectorId"`
		ConnectorToken string `json:"connectorToken"`
	} `json:"config"`
}

// runPair asks the server for a code, waits for the person to enter it in the
// app, then writes connector.json.
//
// The alternative — copying a token out of the app by hand — is the step people
// get wrong, so the code carries the credential instead.
func runPair(serverURL, dataDir string) error {
	serverURL = trimRightSlash(serverURL)

	var started pairingStart
	if err := postJSON(serverURL+"/api/v2/pairing/start", map[string]any{
		"serverUrl": serverURL,
	}, &started); err != nil {
		return fmt.Errorf("could not start pairing: %w", err)
	}

	fmt.Println()
	fmt.Println("  Open the Agents Anywhere app and add a device.")
	fmt.Println("  Enter this code:")
	fmt.Println()
	fmt.Printf("      %s\n", started.Code)
	fmt.Println()
	fmt.Printf("  Waiting for the code to be claimed (expires %s)\n", started.ExpiresAt)

	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		var polled pairingPoll
		if err := postJSON(serverURL+"/api/v2/pairing/poll", map[string]any{
			"pairingId": started.PairingID,
		}, &polled); err != nil {
			return fmt.Errorf("could not poll pairing: %w", err)
		}
		if polled.Config != nil && polled.Config.ConnectorToken != "" {
			profile := connectorFile{
				ServerURL:      polled.Config.ServerURL,
				ConnectorID:    polled.Config.ConnectorID,
				ConnectorToken: polled.Config.ConnectorToken,
			}
			path := filepath.Join(dataDir, "connector.json")
			if err := os.MkdirAll(dataDir, 0o700); err != nil {
				return err
			}
			body, _ := json.MarshalIndent(profile, "", "  ")
			if err := os.WriteFile(path, body, 0o600); err != nil {
				return err
			}
			fmt.Printf("\n  Paired. Wrote %s\n", path)
			fmt.Println("  Start the connector with:")
			fmt.Printf("      aa-connector --config %s\n", path)
			return nil
		}
		if polled.Status == "expired" {
			return fmt.Errorf("the code expired before it was entered")
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("timed out waiting for the code to be entered")
}

// postJSON sends a small JSON body and decodes the reply.
func postJSON(url string, body any, dst any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server answered %d: %s", resp.StatusCode, string(payload))
	}
	return json.Unmarshal(payload, dst)
}
