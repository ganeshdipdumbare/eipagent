// Package symbols exposes the sdk package to interpreted plugins.
package symbols

import "reflect"

//go:generate go run github.com/traefik/yaegi/cmd/yaegi extract github.com/ganeshdipdumbare/eipagent/sdk
//go:generate sed -i.bak -e "/^\\/\\/go:build/d" -e "/^\\/\\/ +build/d" github_com-ganeshdipdumbare-eipagent-sdk.go
//go:generate rm -f github_com-ganeshdipdumbare-eipagent-sdk.go.bak

var Symbols = map[string]map[string]reflect.Value{}
