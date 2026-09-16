// Package jsonstrict rejects ambiguous JSON at the business HTTP and operator
// file boundaries. Callers bound input bytes before invoking Decode.
package jsonstrict

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
)

var errAmbiguous = errors.New("ambiguous JSON")

// Decode rejects unknown fields, duplicate keys, excessive nesting, invalid
// UTF-8 and trailing values before decoding into the typed destination.
func Decode(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errAmbiguous
	}
	check := json.NewDecoder(bytes.NewReader(data))
	if err := scanValue(check, 0); err != nil {
		return err
	}
	if _, err := check.Token(); !errors.Is(err, io.EOF) {
		return errAmbiguous
	}
	shape := json.NewDecoder(bytes.NewReader(data))
	shape.UseNumber()
	var value any
	if err := shape.Decode(&value); err != nil {
		return err
	}
	targetType := reflect.TypeOf(target)
	if targetType == nil || targetType.Kind() != reflect.Pointer {
		return errAmbiguous
	}
	if err := validateShape(value, targetType.Elem()); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

// encoding/json alone accepts case-folded field aliases and null numeric
// elements. Validate the exact typed wire shape before its normal conversion.
func validateShape(value any, target reflect.Type) error {
	if target.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		return validateShape(value, target.Elem())
	}
	if value == nil {
		return errAmbiguous
	}
	if target == reflect.TypeFor[time.Time]() {
		if _, ok := value.(string); !ok {
			return errAmbiguous
		}
		return nil
	}
	switch target.Kind() {
	case reflect.Struct:
		object, ok := value.(map[string]any)
		if !ok {
			return errAmbiguous
		}
		fields := make(map[string]reflect.Type)
		for i := range target.NumField() {
			field := target.Field(i)
			if !field.IsExported() {
				continue
			}
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = field.Name
			}
			fields[name] = field.Type
		}
		for key, child := range object {
			field, exists := fields[key]
			if !exists {
				return errAmbiguous
			}
			if err := validateShape(child, field); err != nil {
				return err
			}
		}
	case reflect.Map:
		object, ok := value.(map[string]any)
		if !ok || target.Key().Kind() != reflect.String {
			return errAmbiguous
		}
		for _, child := range object {
			if err := validateShape(child, target.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		array, ok := value.([]any)
		if !ok {
			return errAmbiguous
		}
		for _, child := range array {
			if err := validateShape(child, target.Elem()); err != nil {
				return err
			}
		}
	case reflect.String:
		if _, ok := value.(string); !ok {
			return errAmbiguous
		}
	case reflect.Bool:
		if _, ok := value.(bool); !ok {
			return errAmbiguous
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		if _, ok := value.(json.Number); !ok {
			return errAmbiguous
		}
	default:
		return errAmbiguous
	}
	return nil
}

func scanValue(decoder *json.Decoder, depth int) error {
	if depth > 32 {
		return errAmbiguous
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return errAmbiguous
			}
			seen[name] = true
			if err := scanValue(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanValue(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errAmbiguous
	}
	_, err = decoder.Token()
	return err
}
