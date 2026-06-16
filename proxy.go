package main

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/net/proxy"
)

type xrayConfig struct {
	Log       json.RawMessage `json:"log"`
	Inbounds  []xrayInbound   `json:"inbounds"`
	Outbounds []xrayOutbound  `json:"outbounds"`
}

type xrayInbound struct {
	Port     int         `json:"port"`
	Protocol string      `json:"protocol"`
	Settings interface{} `json:"settings"`
}

type xrayOutbound struct {
	Protocol      string          `json:"protocol"`
	Settings      json.RawMessage `json:"settings"`
	StreamSettings json.RawMessage `json:"streamSettings"`
}

func startXrayAndProxy() (*http.Client, func()) {
	uuid := os.Getenv("vless_uuid")
	server := os.Getenv("vless_server")
	port := os.Getenv("vless_port")
	sni := os.Getenv("vless_sni")
	fp := os.Getenv("vless_fp")
	pbk := os.Getenv("vless_pbk")
	sid := os.Getenv("vless_sid")

	if uuid == "" || server == "" {
		fmt.Println("VLESS not configured, using direct connection")
		return http.DefaultClient, func() {}
	}

	if port == "" {
		port = "8443"
	}
	if sni == "" {
		sni = "apple.com"
	}
	if fp == "" {
		fp = "firefox"
	}

	socksPort := "10808"
	workDir := getWorkDir()

	// xray dir: try /app/xray first (Docker), then workDir
	xrayDir := filepath.Join(workDir, "xray")
	if _, err := os.Stat(xrayDir); os.IsNotExist(err) {
		xrayDir = workDir
	}

	configJSON := fmt.Sprintf(`{
  "log": {"loglevel": "none"},
  "inbounds": [
    {"port": %s, "protocol": "socks", "settings": {"udp": true}},
    {"port": 10809, "protocol": "http"}
  ],
  "outbounds": [
    {
      "protocol": "vless",
      "settings": {
        "vnext": [
          {
            "address": "%s",
            "port": %s,
            "users": [
              {
                "id": "%s",
                "encryption": "none",
                "flow": ""
              }
            ]
          }
        ]
      },
      "streamSettings": {
        "network": "tcp",
        "security": "reality",
        "realitySettings": {
          "serverName": "%s",
          "fingerprint": "%s",
          "publicKey": "%s",
          "shortId": "%s"
        },
        "tcpSettings": {
          "header": {"type": "none"}
        }
      }
    }
  ]
}`, socksPort, server, port, uuid, sni, fp, pbk, sid)

	configPath := filepath.Join(xrayDir, "xray_config.json")
	os.WriteFile(configPath, []byte(configJSON), 0644)

	// Find xray binary: system path > workDir/xray > download
	xrayBin := "xray"
	xrayPath, err := exec.LookPath(xrayBin)
	if err != nil {
		xrayPath = filepath.Join(xrayDir, xrayBin)
		if _, err := os.Stat(xrayPath); os.IsNotExist(err) {
			xrayPath = filepath.Join(xrayDir, "xray.exe")
			if _, err := os.Stat(xrayPath); os.IsNotExist(err) {
				fmt.Println("Downloading xray-core...")
				if err := downloadXray(xrayPath); err != nil {
					fmt.Printf("Failed to download xray: %v\n", err)
					fmt.Println("Using direct connection")
					return http.DefaultClient, func() {}
				}
			}
		}
	}

	// Start xray
	cmd := exec.Command(xrayPath, "run", "-c", configPath)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		fmt.Printf("Failed to start xray: %v\n")
		return http.DefaultClient, func() {}
	}

	// Wait for SOCKS5 to be ready
	ready := false
	for i := 0; i < 20; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+socksPort, 500*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if !ready {
		fmt.Println("Xray SOCKS5 not ready, using direct connection")
		cmd.Process.Kill()
		return http.DefaultClient, func() {}
	}

	fmt.Printf("Xray started, SOCKS5 on :%s\n", socksPort)

	// Create SOCKS5 dialer
	dialer, err := proxy.SOCKS5("tcp", "127.0.0.1:"+socksPort, nil, proxy.Direct)
	if err != nil {
		fmt.Printf("SOCKS5 dialer error: %v\n", err)
		cmd.Process.Kill()
		return http.DefaultClient, func() {}
	}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.Dial(network, addr)
		},
	}

	client := &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}

	cleanup := func() {
		cmd.Process.Kill()
		os.Remove(configPath)
	}

	return client, cleanup
}

func getWorkDir() string {
	dir, _ := os.Getwd()
	return dir
}

func downloadXray(destPath string) error {
	zipPath := filepath.Join(getWorkDir(), "xray_temp.zip")

	url := "https://github.com/XTLS/Xray-core/releases/download/v25.3.6/Xray-windows-64.zip"
	if runtime.GOOS == "linux" {
		url = "https://github.com/XTLS/Xray-core/releases/download/v25.3.6/Xray-linux-64.zip"
	} else if runtime.GOOS == "darwin" {
		url = "https://github.com/XTLS/Xray-core/releases/download/v25.3.6/Xray-macos-arm64-v8a.zip"
	}

	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil
		},
		Timeout: 60 * time.Second,
	}

	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	io.Copy(out, resp.Body)
	out.Close()

	// Extract xray.exe from zip
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()

	for _, file := range reader.Reader.File {
		name := filepath.Base(file.Name)
		if name == "xray" || name == "xray.exe" {
			rc, err := file.Open()
			if err != nil {
				return err
			}
			defer rc.Close()

			out, err = os.Create(destPath)
			if err != nil {
				return err
			}
			io.Copy(out, rc)
			out.Close()

			os.Remove(zipPath)
			return nil
		}
	}

	os.Remove(zipPath)
	return fmt.Errorf("xray not found in archive")
}
