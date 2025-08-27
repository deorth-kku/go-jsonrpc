package jsonrpc

import "reflect"

type Object[T any] struct {
	Value T `json:",inline"`
}

func (Object[T]) isObject() {}

type isObject interface {
	isObject()
}

var (
	_            isObject = (Object[struct{}]{})
	isObjectType          = reflect.TypeFor[isObject]()
)
