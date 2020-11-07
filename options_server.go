package jsonrpc

import (
	"context"
	"reflect"
	"syscall/js"
)

type ParamDecoder func(ctx context.Context, json []byte) (reflect.Value, error)

type ServerConfig struct {
	paramDecoders   map[reflect.Type]ParamDecoder
	connectCallback func(environment js.Value)
}

type ServerOption func(c *ServerConfig)

func defaultServerConfig() ServerConfig {
	return ServerConfig{
		paramDecoders:   map[reflect.Type]ParamDecoder{},
		connectCallback: nil,
	}
}

func WithParamDecoder(t interface{}, decoder ParamDecoder) ServerOption {
	return func(c *ServerConfig) {
		c.paramDecoders[reflect.TypeOf(t).Elem()] = decoder
	}
}
