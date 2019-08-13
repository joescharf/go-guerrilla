package guerrilla

import (
	"bytes"
)

type authenticationCommand []byte

var (
	// Required the username
	cmdAuthUsername authenticationCommand = []byte("authUsername")
	// Required the password
	cmdAuthPassword authenticationCommand = []byte("authPassword")
)

func (c authenticationCommand) match(in []byte) bool {
	return bytes.Index(in, []byte(c)) == 0
}

type LoginInfo struct {
	username string
	password string
	status   bool
}

type ValidateCallbackFunc func(username string, password string) bool

var (
	Authentication = &AuthenticationValidator{}
)

type AuthenticationValidator struct {
	handleFunctions []ValidateCallbackFunc
}

func (v *AuthenticationValidator) AddValidator(f ValidateCallbackFunc) {
	v.handleFunctions = append(v.handleFunctions, f)
}

func (v *AuthenticationValidator) Validate(a *LoginInfo) bool {
	for _, f := range v.handleFunctions {
		isValid := f(a.username, a.password)
		if !isValid {
			return false
		}
	}
	return true
}
