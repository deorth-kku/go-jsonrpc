package auth

import (
	"context"
	"fmt"
	"reflect"
	"slices"

	"github.com/deorth-kku/go-common"
)

type Permission string

type permKey int

var permCtxKey permKey

func WithPerm(ctx context.Context, perms []Permission) context.Context {
	return context.WithValue(ctx, permCtxKey, perms)
}

func HasPerm(ctx context.Context, defaultPerms []Permission, perm Permission) bool {
	callerPerms, ok := ctx.Value(permCtxKey).([]Permission)
	if !ok {
		callerPerms = defaultPerms
	}

	return slices.Contains(callerPerms, perm)
}

func PermissionedProxy(validPerms, defaultPerms []Permission, in any, out any) {
	rint := reflect.ValueOf(out).Elem()
	ra := reflect.ValueOf(in)

	for f := range rint.NumField() {
		field := rint.Type().Field(f)
		requiredPerm := Permission(field.Tag.Get("perm"))
		if requiredPerm == "" {
			panic("missing 'perm' tag on " + field.Name) // ok
		}

		// Validate perm tag
		ok := slices.Contains(validPerms, requiredPerm)
		if !ok {
			panic("unknown 'perm' tag on " + field.Name) // ok
		}

		fn := ra.MethodByName(field.Name)

		rint.Field(f).Set(reflect.MakeFunc(field.Type, func(args []reflect.Value) (results []reflect.Value) {
			ctx := common.MustOk(reflect.TypeAssert[context.Context](args[0]))
			if HasPerm(ctx, defaultPerms, requiredPerm) {
				return fn.Call(args)
			}

			err := fmt.Errorf("missing permission to invoke '%s' (need '%s')", field.Name, requiredPerm)
			rerr := reflect.ValueOf(&err).Elem()

			if field.Type.NumOut() == 2 {
				return []reflect.Value{
					reflect.Zero(field.Type.Out(0)),
					rerr,
				}
			} else {
				return []reflect.Value{rerr}
			}
		}))

	}
}
