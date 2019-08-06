package guerrilla

import (
	"bytes"
	"encoding/base64"
)

type authCommand []byte

var (
	// Required the username
	cmdAuthUsername authCommand = []byte("authUsername")
	// Required the password
	cmdAuthPassword authCommand = []byte("authPassword")
)

func (c authCommand) match(in []byte) bool {
	return bytes.Index(in, []byte(c)) == 0
}

type Auth struct {
	username string
	password string
	status   bool
}

type VaildateCallbackFunc func(username string, password string) bool

var (
	Validator = &AuthVaildator{}
)

type AuthVaildator struct {
	handleFunctions []VaildateCallbackFunc
}

func (v *AuthVaildator) AddAuthVaildator(f VaildateCallbackFunc) {
	v.handleFunctions = append(v.handleFunctions, f)
}

func (v *AuthVaildator) Vaildate(a *Auth) bool {
	for _, f := range v.handleFunctions {
		username, err := base64.StdEncoding.DecodeString(a.username)
		if err != nil {
			return false
		}

		password, err := base64.StdEncoding.DecodeString(a.password)
		if err != nil {
			return false
		}

		isValidate := f(string(username), string(password))
		if isValidate == false {
			return isValidate
		}
	}
	return true
}
