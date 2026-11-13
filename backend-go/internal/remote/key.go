package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

// A key file is the one place a remote credential lives in the workspace layout:
// G/auth/keys/<key>.json, mode 0600, naming the binding it is for and holding the token itself
// or the JAD_REMOTE_* variable that does. G/auth is hidden in every sandbox. The file is read at
// each request and never cached, logged, shown or put in an error: errors name the key and the
// binding only.

type keyFile struct {
	SchemaVersion int    `json:"schema_version"`
	Kind          string `json:"kind"`
	Binding       string `json:"binding"`
	Token         string `json:"token"`
	TokenEnv      string `json:"token_env"`
}

// KeyBinding reads a key file's structure, without keeping or checking the secret, and returns
// the binding it is for. It is what a session load uses to resolve remote_keys.
func KeyBinding(name, path string) (string, error) {
	data, err := readKeyFile(name, path)
	if err != nil {
		return "", err
	}
	file, err := parseKey(name, data)
	if err != nil {
		return "", err
	}
	return file.Binding, nil
}

func parseKey(name string, data []byte) (keyFile, error) {
	var file keyFile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return keyFile{}, fmt.Errorf("key %s: the key file is not a valid remote_key file", name)
	}
	switch {
	case file.SchemaVersion != 0 && file.SchemaVersion != 1:
		return keyFile{}, fmt.Errorf("key %s: unsupported schema_version %d", name, file.SchemaVersion)
	case file.Kind != "remote_key":
		return keyFile{}, fmt.Errorf(`key %s: kind must be "remote_key"`, name)
	case !NamePattern.MatchString(file.Binding):
		return keyFile{}, fmt.Errorf("key %s: binding must name a remote binding (1 to 64 letters, digits, '_' or '-')", name)
	case file.Token != "" && file.TokenEnv != "":
		return keyFile{}, fmt.Errorf("key %s: token and token_env are alternatives: set one", name)
	case file.Token == "" && file.TokenEnv == "":
		return keyFile{}, fmt.Errorf("key %s: a key needs token or token_env", name)
	case file.TokenEnv != "" && !envNamePattern.MatchString(file.TokenEnv):
		return keyFile{}, fmt.Errorf("key %s: token_env must be an environment variable named JAD_REMOTE_<something>: a key may not read other variables such as provider keys", name)
	}
	return file, nil
}

// readKeyFile opens the file once and checks the descriptor, so the file that is checked is the
// file that is read: a regular file under maxTokenFile bytes, closed to group and others.
func readKeyFile(name, path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("key %s: the key file cannot be read", name)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("key %s: the key file cannot be read", name)
	}
	if !info.Mode().IsRegular() || info.Size() > maxTokenFile {
		return nil, fmt.Errorf("key %s: the key file must be a regular file under %d bytes", name, maxTokenFile)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("key %s: the key file must not be readable by group or others (chmod 600)", name)
	}
	tokenFileChecked()
	data, err := io.ReadAll(io.LimitReader(file, maxTokenFile+1))
	if err != nil || len(data) > maxTokenFile {
		return nil, fmt.Errorf("key %s: the key file cannot be read", name)
	}
	return data, nil
}

// keyToken is the token of the entry's key for this binding. A key for another binding is
// refused, so a key never works on a binding it was not written for.
func (e Entry) keyToken(environment func(string) string) (string, error) {
	if e.KeyFile == "" {
		return "", fmt.Errorf("remote %s: this session has no key for it", e.Name)
	}
	data, err := readKeyFile(e.KeyName, e.KeyFile)
	if err != nil {
		return "", fmt.Errorf("remote %s: %v", e.Name, err)
	}
	file, err := parseKey(e.KeyName, data)
	if err != nil {
		return "", fmt.Errorf("remote %s: %v", e.Name, err)
	}
	if file.Binding != e.Name {
		return "", fmt.Errorf("remote %s: key %s is for another binding", e.Name, e.KeyName)
	}
	token := strings.TrimSpace(file.Token)
	if file.TokenEnv != "" {
		if environment == nil {
			environment = os.Getenv
		}
		if token = strings.TrimSpace(environment(file.TokenEnv)); token == "" {
			return "", fmt.Errorf("remote %s: key %s: environment variable %s is not set", e.Name, e.KeyName, file.TokenEnv)
		}
	}
	if len(token) < MinTokenBytes || !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("remote %s: key %s: the token must be at least %d printable characters without spaces", e.Name, e.KeyName, MinTokenBytes)
	}
	return token, nil
}
