// Package symbols exposes eipagent/sdk to interpreted plugins.
package symbols

import "reflect"

//go:generate go run github.com/traefik/yaegi/cmd/yaegi extract eipagent/sdk
//go:generate sed -i.bak -e "/^\\/\\/go:build/d" -e "/^\\/\\/ +build/d" eipagent-sdk.go
//go:generate rm -f eipagent-sdk.go.bak

var Symbols = map[string]map[string]reflect.Value{}
