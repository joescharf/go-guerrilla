package guerrilla

import "bytes"

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
		isValidate := f(a.username, a.password)
		if isValidate == false {
			return isValidate
		}
	}
	return true
}
