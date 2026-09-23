// Package config decodes service configuration from the environment into
// tagged structs. Defaults live in Go code, the environment only overrides
// them, the same way the platform does it.
//
// A field is bound to the environment by an env tag holding one variable name
// or a chain of names separated by ";". Every name of the chain is looked up
// and the last one that is set wins, so a service specific name overrides a
// global one. The option ",required" rejects an unset or empty variable.
//
//	Endpoint  string `env:"OC_EVENTS_ENDPOINT" desc:"NATS of the platform."`
//	Level     string `env:"OC_LOG_LEVEL;FILE_ACTIVITY_LOG_LEVEL" desc:"Log level."`
//	AccountID string `env:"OC_SERVICE_ACCOUNT_ID,required" desc:"Service account."`
package config

import (
	"encoding"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ChainSeparator separates the variable names of an env tag.
const ChainSeparator = ";"

// Errors returned while decoding a configuration.
var (
	ErrInvalidTarget = errors.New("config: target must be a non-nil pointer to a struct")
	ErrMissing       = errors.New("config: variable must be set")
	ErrUnsupported   = errors.New("config: unsupported field type")
)

// Validator is implemented by configurations that check themselves once the
// environment has been applied. Decode calls Validate last.
type Validator interface {
	Validate() error
}

// Decode applies the environment to the fields of cfg, which must be a
// non-nil pointer to a struct. Untagged struct fields are walked through.
// All problems are collected, so one run reports everything wrong with the
// environment.
func Decode(cfg any) error {
	value := reflect.ValueOf(cfg)
	if value.Kind() != reflect.Pointer || value.IsNil() || value.Elem().Kind() != reflect.Struct {
		return ErrInvalidTarget
	}

	var errs []error
	decodeStruct(value.Elem(), &errs)
	if err := errors.Join(errs...); err != nil {
		return err
	}

	if validator, ok := cfg.(Validator); ok {
		return validator.Validate()
	}
	return nil
}

func decodeStruct(target reflect.Value, errs *[]error) {
	structType := target.Type()
	for i := range structType.NumField() {
		field, value := structType.Field(i), target.Field(i)

		tag, tagged := field.Tag.Lookup("env")
		if !tagged {
			// An embedded struct is unsettable itself while its exported
			// fields are settable, so it is walked through before the check.
			if value.Kind() == reflect.Struct {
				decodeStruct(value, errs)
			}
			continue
		}
		if !value.CanSet() {
			continue
		}

		chain, required := strings.CutSuffix(tag, ",required")
		name, raw, found := lookup(chain)
		if !found || (required && raw == "") {
			if required {
				*errs = append(*errs, fmt.Errorf("%w: %s", ErrMissing, chain))
			}
			continue
		}

		if err := setValue(value, raw); err != nil {
			*errs = append(*errs, fmt.Errorf("config: %s=%q: %w", name, raw, err))
		}
	}
}

// lookup resolves a chain of names and reports which one was used.
func lookup(chain string) (string, string, bool) {
	names := strings.Split(chain, ChainSeparator)
	name, value, found := names[len(names)-1], "", false
	for _, candidate := range names {
		if v, ok := os.LookupEnv(strings.TrimSpace(candidate)); ok {
			name, value, found = candidate, v, true
		}
	}
	return name, value, found
}

func setValue(target reflect.Value, raw string) error {
	if unmarshaler, ok := target.Addr().Interface().(encoding.TextUnmarshaler); ok {
		return unmarshaler.UnmarshalText([]byte(raw))
	}

	switch target.Kind() {
	case reflect.String:
		target.SetString(raw)
	case reflect.Bool:
		parsed, err := strconv.ParseBool(raw)
		if err != nil {
			return errors.New("must be a boolean")
		}
		target.SetBool(parsed)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return setInt(target, raw)
	case reflect.Slice:
		return setSlice(target, raw)
	default:
		return fmt.Errorf("%w %s", ErrUnsupported, target.Type())
	}
	return nil
}

func setInt(target reflect.Value, raw string) error {
	if target.Type() == reflect.TypeFor[time.Duration]() {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return errors.New("must be a duration, like 30s or 90h")
		}
		target.SetInt(int64(parsed))
		return nil
	}

	parsed, err := strconv.ParseInt(raw, 10, target.Type().Bits())
	if err != nil {
		return errors.New("must be an integer")
	}
	target.SetInt(parsed)
	return nil
}

// setSlice fills a slice from a comma separated value, dropping empty items
// and trimming spaces the way the platform does.
func setSlice(target reflect.Value, raw string) error {
	if target.Type().Elem().Kind() != reflect.String {
		return fmt.Errorf("%w %s", ErrUnsupported, target.Type())
	}

	items := []string{}
	for item := range strings.SplitSeq(raw, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}

	slice := reflect.MakeSlice(target.Type(), len(items), len(items))
	for i, item := range items {
		slice.Index(i).SetString(item)
	}
	target.Set(slice)
	return nil
}
