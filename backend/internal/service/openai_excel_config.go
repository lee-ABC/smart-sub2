package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type ExcelRoutingConfig struct {
	GatewayURL       string            `json:"gateway_url"`
	TransportKeyFile string            `json:"transport_key_file"`
	GroupModes       map[string]string `json:"group_modes"`
}

var excelConfigMu sync.Mutex

func excelConfigPath() string {
	if p := os.Getenv("SUB2_EXCEL_CONFIG_FILE"); p != "" {
		return p
	}
	return "/app/data/excel-routing.json"
}
func ReadExcelRoutingConfig() (*ExcelRoutingConfig, error) {
	raw, err := os.ReadFile(excelConfigPath())
	if errors.Is(err, os.ErrNotExist) {
		return &ExcelRoutingConfig{GroupModes: map[string]string{}}, nil
	}
	if err != nil || len(raw) > 65536 {
		return nil, errors.New("Excel routing configuration unavailable")
	}
	var cfg ExcelRoutingConfig
	if json.Unmarshal(raw, &cfg) != nil {
		return nil, errors.New("Invalid Excel routing configuration")
	}
	for id, mode := range cfg.GroupModes {
		n, e := strconv.ParseInt(id, 10, 64)
		if e != nil || n <= 0 || (mode != "native" && mode != "excel") {
			return nil, errors.New("Invalid Excel group mode")
		}
	}
	return &cfg, nil
}
func (c *ExcelRoutingConfig) Mode(groupID int64) string {
	if c != nil && c.GroupModes[strconv.FormatInt(groupID, 10)] == "excel" {
		return "excel"
	}
	return "native"
}
func (c *ExcelRoutingConfig) TransportKey() (string, error) {
	u, err := url.Parse(c.GatewayURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.TrimRight(u.Path, "/") != "/internal/v1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("Excel gateway is not configured")
	}
	b, err := os.ReadFile(c.TransportKeyFile)
	key := strings.TrimSpace(string(b))
	if err != nil || len(key) < 32 || len(key) > 256 {
		return "", errors.New("Excel transport credential unavailable")
	}
	for _, r := range key {
		if r < 33 || r > 126 {
			return "", errors.New("Invalid Excel transport credential")
		}
	}
	return key, nil
}
func SetExcelGroupMode(groupID int64, mode string) error {
	if groupID <= 0 || (mode != "native" && mode != "excel") {
		return errors.New("Invalid group mode")
	}
	excelConfigMu.Lock()
	defer excelConfigMu.Unlock()
	cfg, err := ReadExcelRoutingConfig()
	if err != nil {
		return err
	}
	if mode == "excel" {
		if _, err := cfg.TransportKey(); err != nil {
			return err
		}
	}
	if cfg.GroupModes == nil {
		cfg.GroupModes = map[string]string{}
	}
	cfg.GroupModes[strconv.FormatInt(groupID, 10)] = mode
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	p := excelConfigPath()
	f, err := os.CreateTemp(filepath.Dir(p), ".excel-routing-*")
	if err != nil {
		return errors.New("Cannot save Excel group mode")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), p)
	}
	if err != nil {
		return fmt.Errorf("Cannot persist Excel group mode: %w", err)
	}
	return nil
}

// ExcelModelIDs is the shared public and admin-probe catalog.
func ExcelModelIDs() []string {
	return []string{"gpt-6-sol", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna"}
}

// Models outside the Excel adapter retain the native transport.
func IsExcelSupportedModel(model string) bool {
	switch strings.TrimSpace(model) {
	case "gpt-6-sol", "gpt-6-astra", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna",
		"gpt-6-excel", "gpt-6-astra-excel", "gpt-5.6-sol-excel", "gpt-5.6-terra-excel", "gpt-5.6-luna-excel":
		return true
	default:
		return false
	}
}
func excelRoutingModel(account *Account, model, dispatchModel string) string {
	if account != nil && account.IsOpenAIPassthroughEnabled() {
		return model
	}
	return normalizeOpenAIModelForUpstream(account, resolveOpenAIForwardModel(account, model, dispatchModel))
}
