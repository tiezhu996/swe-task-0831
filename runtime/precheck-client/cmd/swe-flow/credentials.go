package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type platformCredentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Session  string `json:"session"`
	CSRF     string `json:"csrf"`
}

func readPrivateCredentials(path string) (platformCredentials, error) {
	var config struct {
		SchemaVersion int                 `json:"schema_version"`
		SWE           platformCredentials `json:"swe"`
	}
	for _, target := range []string{filepath.Dir(path), path} {
		info, err := os.Lstat(target)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return platformCredentials{}, fmt.Errorf("私有认证配置不存在或权限不安全（目录0700、文件0600）")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &config) != nil || config.SchemaVersion != 1 {
		return platformCredentials{}, fmt.Errorf("私有认证配置格式不合法；不输出凭据")
	}
	c := config.SWE
	if (c.Username == "" || c.Password == "") && (c.Session == "" || c.CSRF == "") {
		return platformCredentials{}, fmt.Errorf("私有认证配置缺少 SWE 凭据")
	}
	return c, nil
}

func localCredentials() (platformCredentials, error) {
	c := platformCredentials{Username: os.Getenv("SWE_USERNAME"), Password: os.Getenv("SWE_PASSWORD"), Session: os.Getenv("SWE_SESSION"), CSRF: os.Getenv("SWE_CSRF")}
	if (c.Username != "" && c.Password != "") || (c.Session != "" && c.CSRF != "") {
		return c, nil
	}
	store, err := readSWEAccountStore()
	if err == nil {
		if account := store.activeAccount(); account != nil {
			return account.credentials(), nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	path := sweConfigPath()
	if path == "" {
		return c, fmt.Errorf("无法定位本机私有认证配置")
	}
	return readPrivateCredentials(path)
}
