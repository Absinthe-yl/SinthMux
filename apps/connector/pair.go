package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sinthmux/sinthmux/internal/config"
)

func pairedConfigPath() (string, error) {
	if path := os.Getenv("SINTHMUX_CONNECTOR_CONFIG"); path != "" {
		return path, nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "sinthmux", "connector.json"), nil
}

func loadPairedConfig() (config.Connector, error) {
	path, err := pairedConfigPath()
	if err != nil {
		return config.Connector{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return config.Connector{}, err
	}
	var saved config.Connector
	if err := json.Unmarshal(data, &saved); err != nil {
		return config.Connector{}, err
	}
	if saved.DeviceID == "" || saved.DeviceToken == "" || saved.HubURL == "" {
		return config.Connector{}, errors.New("设备配置不完整")
	}
	return saved, nil
}

func pair(args []string) error {
	flags := flag.NewFlagSet("pair", flag.ContinueOnError)
	hub := flags.String("hub", "", "Hub 的公开 HTTPS 地址")
	code := flags.String("code", "", "网页生成的一次性配对码")
	reuseExisting := flags.Bool("reuse-existing", false, "同一 Hub 已配对时保留现有设备身份")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("不支持额外参数")
	}
	endpoint, err := url.Parse(*hub)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "" || (endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && (endpoint.Hostname() == "localhost" || endpoint.Hostname() == "127.0.0.1"))) {
		return errors.New("Hub 地址必须是 HTTPS；仅本机可用 HTTP")
	}
	path, err := pairedConfigPath()
	if err != nil {
		return err
	}
	if _, err = os.Stat(path); err == nil {
		if !*reuseExisting {
			return fmt.Errorf("设备已有配置：%s；请先移走旧配置再配对", path)
		}
		saved, loadErr := loadPairedConfig()
		if loadErr != nil {
			return fmt.Errorf("无法复用已有设备配置：%w", loadErr)
		}
		savedHub, parseErr := url.Parse(saved.HubURL)
		expectedScheme := "wss"
		if endpoint.Scheme == "http" {
			expectedScheme = "ws"
		}
		if parseErr != nil || savedHub.Scheme != expectedScheme || !strings.EqualFold(savedHub.Host, endpoint.Host) || savedHub.Path != "/ws/v1/connectors/connect" {
			return fmt.Errorf("设备已配对到其他 Hub；原配置保留在 %s", path)
		}
		fmt.Printf("设备 %s 已配对到此 Hub，保留原设备身份并更新设备代理\n", saved.Name)
		if *code != "" {
			fmt.Println("提示：这次输入的配对码不会使用；本机仍显示为原设备。")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if *code == "" {
		return errors.New("首次接入需要网页生成的一次性配对码")
	}
	requestBody, _ := json.Marshal(map[string]string{"code": *code})
	endpoint.Path = "/api/v1/connectors/pair"
	request, err := http.NewRequest(http.MethodPost, endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return fmt.Errorf("Hub 拒绝配对（HTTP %d），请重新生成配对命令", response.StatusCode)
	}
	var paired struct{ DeviceID, DeviceToken, HubURL, Name string }
	if err := json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&paired); err != nil {
		return err
	}
	if paired.DeviceID == "" || !strings.HasPrefix(paired.DeviceToken, "smd_"+paired.DeviceID+"_") || paired.HubURL == "" {
		return errors.New("Hub 返回的设备配置无效")
	}
	saved := config.Connector{DeviceID: paired.DeviceID, DeviceToken: paired.DeviceToken, HubURL: paired.HubURL, Name: paired.Name}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(file).Encode(saved); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	fmt.Printf("设备 %s 已配对，配置保存在 %s\n", paired.Name, path)
	return nil
}
