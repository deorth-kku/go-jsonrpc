package jsonrpc

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"log/slog"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
)

type ComplexData struct {
	Foo string `json:"foo"`
	Bar int    `json:"bar"`
}

type StaticError struct{}

func (e *StaticError) Error() string { return "static error" }

// Define the error types
type SimpleError struct {
	Message string
}

func (e *SimpleError) Error() string {
	return e.Message
}

func (e *SimpleError) FromJSONRPCError(jerr JSONRPCError) error {
	e.Message = jerr.Message
	return nil
}

func (e *SimpleError) ToJSONRPCError() (JSONRPCError, error) {
	return JSONRPCError{Message: e.Message}, nil
}

var _ RPCErrorCodec = (*SimpleError)(nil)

type DataStringError struct {
	Message string `json:"message"`
	Data    string `json:"data"`
}

func (e *DataStringError) Error() string {
	return e.Message
}

func (e *DataStringError) FromJSONRPCError(jerr JSONRPCError) error {
	e.Message = jerr.Message
	data, ok := jerr.Data.(string)
	if !ok {
		return fmt.Errorf("expected string data, got %T", jerr.Data)
	}

	e.Data = data

	return nil
}

func (e *DataStringError) ToJSONRPCError() (JSONRPCError, error) {
	return JSONRPCError{Message: e.Message, Data: e.Data}, nil
}

var _ RPCErrorCodec = (*DataStringError)(nil)

type DataComplexError struct {
	Message      string
	internalData ComplexData
}

func (e *DataComplexError) Error() string {
	return e.Message
}

func (e *DataComplexError) FromJSONRPCError(jerr JSONRPCError) error {
	e.Message = jerr.Message
	data, ok := jerr.Data.(jsontext.Value)
	if !ok {
		return fmt.Errorf("expected string data, got %T", jerr.Data)
	}

	if err := json.Unmarshal(data, &e.internalData, v1opts); err != nil {
		return err
	}
	return nil
}

func (e *DataComplexError) ToJSONRPCError() (JSONRPCError, error) {
	data, err := json.Marshal(e.internalData, v1opts)
	if err != nil {
		return JSONRPCError{}, err
	}
	return JSONRPCError{Message: e.Message, Data: data}, nil
}

var _ RPCErrorCodec = (*DataComplexError)(nil)

type MetaError struct {
	Message string
	Details string
}

func (e *MetaError) Error() string {
	return e.Message
}

func (e *MetaError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Message string `json:"message"`
		Details string `json:"details"`
	}{
		Message: e.Message,
		Details: e.Details,
	}, v1opts)
}

func (e *MetaError) UnmarshalJSON(data []byte) error {
	var temp struct {
		Message string `json:"message"`
		Details string `json:"details"`
	}
	if err := json.Unmarshal(data, &temp, v1opts); err != nil {
		return err
	}

	e.Message = temp.Message
	e.Details = temp.Details
	return nil
}

type ComplexError struct {
	Message string
	Data    ComplexData
	Details string
}

func (e *ComplexError) Error() string {
	return e.Message
}

func (e *ComplexError) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Message string `json:"message"`
		Details string `json:"details"`
		Data    any    `json:"data"`
	}{
		Details: e.Details,
		Message: e.Message,
		Data:    e.Data,
	}, v1opts)
}

func (e *ComplexError) UnmarshalJSON(data []byte) error {
	var temp struct {
		Message string      `json:"message"`
		Details string      `json:"details"`
		Data    ComplexData `json:"data"`
	}
	if err := json.Unmarshal(data, &temp, v1opts); err != nil {
		return err
	}
	e.Details = temp.Details
	e.Message = temp.Message
	e.Data = temp.Data
	return nil
}

func TestRespErrorVal(t *testing.T) {
	// Initialize the Errors struct and register error types
	errorsMap := NewErrors()
	RegisterError[*StaticError](errorsMap, 1000)
	RegisterError[*SimpleError](errorsMap, 1001)
	RegisterError[*DataStringError](errorsMap, 1002)
	RegisterError[*DataComplexError](errorsMap, 1003)
	RegisterError[*MetaError](errorsMap, 1004)
	RegisterError[*ComplexError](errorsMap, 1005)

	// Define test cases
	testCases := []struct {
		name            string
		respError       *JSONRPCError
		expectedType    any
		expectedMessage string
		verify          func(t *testing.T, err error)
	}{
		{
			name: "StaticError",
			respError: &JSONRPCError{
				Code:    1000,
				Message: "this is ignored",
			},
			expectedType:    &StaticError{},
			expectedMessage: "static error",
		},
		{
			name: "SimpleError",
			respError: &JSONRPCError{
				Code:    1001,
				Message: "simple error occurred",
			},
			expectedType:    &SimpleError{},
			expectedMessage: "simple error occurred",
		},
		{
			name: "DataStringError",
			respError: &JSONRPCError{
				Code:    1002,
				Message: "data error occurred",
				Data:    "additional data",
			},
			expectedType:    &DataStringError{},
			expectedMessage: "data error occurred",
			verify: func(t *testing.T, err error) {
				require.IsType(t, &DataStringError{}, err)
				require.Equal(t, "data error occurred", err.Error())
				require.Equal(t, "additional data", err.(*DataStringError).Data)
			},
		},
		{
			name: "DataComplexError",
			respError: &JSONRPCError{
				Code:    1003,
				Message: "data error occurred",
				Data:    jsontext.Value(`{"foo":"boop","bar":101}`),
			},
			expectedType:    &DataComplexError{},
			expectedMessage: "data error occurred",
			verify: func(t *testing.T, err error) {
				require.Equal(t, ComplexData{Foo: "boop", Bar: 101}, err.(*DataComplexError).internalData)
			},
		},
		{
			name: "MetaError",
			respError: &JSONRPCError{
				Code:    1004,
				Message: "meta error occurred",
				Meta: func() jsontext.Value {
					me := &MetaError{
						Message: "meta error occurred",
						Details: "meta details",
					}
					metaData, _ := me.MarshalJSON()
					return metaData
				}(),
			},
			expectedType:    &MetaError{},
			expectedMessage: "meta error occurred",
			verify: func(t *testing.T, err error) {
				// details will also be included in the error message since it implements the marshable interface
				require.Equal(t, "meta details", err.(*MetaError).Details)
			},
		},
		{
			name: "ComplexError",
			respError: &JSONRPCError{
				Code:    1005,
				Message: "complex error occurred",
				Data:    jsontext.Value(`"complex data"`),
				Meta: func() jsontext.Value {
					ce := &ComplexError{
						Message: "complex error occurred",
						Details: "complex details",
						Data:    ComplexData{Foo: "foo", Bar: 42},
					}
					metaData, _ := ce.MarshalJSON()
					return metaData
				}(),
			},
			expectedType:    &ComplexError{},
			expectedMessage: "complex error occurred",
			verify: func(t *testing.T, err error) {
				require.Equal(t, ComplexData{Foo: "foo", Bar: 42}, err.(*ComplexError).Data)
				require.Equal(t, "complex details", err.(*ComplexError).Details)
			},
		},
		{
			name: "UnregisteredError",
			respError: &JSONRPCError{
				Code:    9999,
				Message: "unregistered error occurred",
				Data:    jsontext.Value(`"some data"`),
			},
			expectedType:    &JSONRPCError{},
			expectedMessage: "unregistered error occurred",
			verify: func(t *testing.T, err error) {
				require.Equal(t, jsontext.Value(`"some data"`), err.(*JSONRPCError).Data)
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			errValue := tc.respError.val(&errorsMap, jsonDefault(), slog.Default())
			err, ok := reflect.TypeAssert[error](errValue)
			require.True(t, ok, "returned value does not implement error interface")
			require.IsType(t, tc.expectedType, err)
			require.Equal(t, tc.expectedMessage, err.Error())
			if tc.verify != nil {
				tc.verify(t, err)
			}
		})
	}
}
