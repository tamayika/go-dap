// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// gentypes generates Go types from debugProtocol.json
//
// Usage:
//
// $ gentypes <path to debugProtocol.json>
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/format"
	"io/ioutil"
	"log"
	"os"
	"strings"
)

// parseRef parses the value of a "$ref" key.
// For example "#definitions/ProtocolMessage" => "ProtocolMessage".
func parseRef(refValue interface{}) string {
	refContents := refValue.(string)
	if !strings.HasPrefix(refContents, "#/definitions/") {
		log.Fatal("want ref to start with '#/definitions/', got ", refValue)
	}

	return replaceGoTypename(refContents[14:])
}

// goFieldName converts a property name from its JSON representation to an
// exported Go field name.
// For example "__some_property_name" => "SomePropertyName".
func goFieldName(jsonPropName string) string {
	clean := strings.ReplaceAll(jsonPropName, "_", " ")
	titled := strings.Title(clean)
	return strings.ReplaceAll(titled, " ", "")
}

// parsePropertyType takes the JSON value of a property field and extracts
// the Go type of the property. For example, given this map:
//
//  {
//    "type": "string",
//    "description": "The command to execute."
//  },
//
// It will emit "string".
func parsePropertyType(propValue map[string]interface{}) string {
	if ref, ok := propValue["$ref"]; ok {
		return parseRef(ref)
	}

	propType, ok := propValue["type"]
	if !ok {
		log.Fatal("property with no type or ref:", propValue)
	}

	switch propType.(type) {
	case string:
		switch propType {
		case "string":
			return "string"
		case "integer":
			return "int"
		case "boolean":
			return "bool"
		case "array":
			propItems, ok := propValue["items"]
			if !ok {
				log.Fatal("missing items type for property of array type:", propValue)
			}
			propItemsMap := propItems.(map[string]interface{})
			return "[]" + parsePropertyType(propItemsMap)
		case "object":
			// When the type of a property is "object", we'll emit a map with a string
			// key and a value type that depends on the type of the
			// additionalProperties field.
			additionalProps, ok := propValue["additionalProperties"]
			if !ok {
				log.Fatal("missing additionalProperties field when type=object:", propValue)
			}
			valueType := parsePropertyType(additionalProps.(map[string]interface{}))
			return fmt.Sprintf("map[string]%v", valueType)
		default:
			log.Fatal("unknown property type value", propType)
		}

	case []interface{}:
		return "interface{}"

	default:
		log.Fatal("unknown property type", propType)
	}

	panic("unreachable")
}

// maybeParseInheritance helps parse types that inherit from other types.
// A type description can have an "allOf" key, which means it inherits from
// another type description. Returns the name of the base type specified in
// allOf, and the description of the inheriting type.
//
// Example:
//
//    "allOf": [ { "$ref": "#/definitions/ProtocolMessage" },
//               {... type description ...} ]
//
// Returns base type ProtocolMessage and a map representing type description.
// If there is no "allOf", returns an empty baseTypeName and descMap itself.
func maybeParseInheritance(descMap map[string]json.RawMessage) (baseTypeName string, typeDescJson map[string]json.RawMessage) {
	allOfListJson, ok := descMap["allOf"]
	if !ok {
		return "", descMap
	}

	var allOfSliceOfJson []json.RawMessage
	if err := json.Unmarshal(allOfListJson, &allOfSliceOfJson); err != nil {
		log.Fatal(err)
	}
	if len(allOfSliceOfJson) != 2 {
		log.Fatal("want 2 elements in allOf list, got", allOfSliceOfJson)
	}

	var baseTypeRef map[string]interface{}
	if err := json.Unmarshal(allOfSliceOfJson[0], &baseTypeRef); err != nil {
		log.Fatal(err)
	}

	if err := json.Unmarshal(allOfSliceOfJson[1], &typeDescJson); err != nil {
		log.Fatal(err)
	}
	return parseRef(baseTypeRef["$ref"]), typeDescJson
}

// emitToplevelType emits a single type into a string. It takes the type name
// and a serialized json object representing the type. The json representation
// will have fields: "type", "properties" etc.
func emitToplevelType(typeName string, descJson json.RawMessage) string {
	var b strings.Builder
	var baseType string

	// We don't parse the description all the way to map[string]interface{}
	// because we have to retain the original JSON-order of properties (in this
	// type as well as any nested types like "body").
	var descMap map[string]json.RawMessage
	if err := json.Unmarshal(descJson, &descMap); err != nil {
		log.Fatal(err)
	}
	baseType, descMap = maybeParseInheritance(descMap)

	typeJson, ok := descMap["type"]
	if !ok {
		log.Fatal("want description to have 'type', got ", descMap)
	}

	var descTypeString string
	if err := json.Unmarshal(typeJson, &descTypeString); err != nil {
		log.Fatal(err)
	}

	if descTypeString == "string" {
		fmt.Fprintf(&b, "type %s string\n", typeName)
		return b.String()
	} else if descTypeString == "object" {
		fmt.Fprintf(&b, "type %s struct {\n", typeName)
		if len(baseType) > 0 {
			fmt.Fprintf(&b, "\t%s\n\n", baseType)
		}
	} else {
		log.Fatal("want description type to be object or string, got ", descTypeString)
	}

	var propsMapOfJson map[string]json.RawMessage
	if propsJson, ok := descMap["properties"]; ok {
		if err := json.Unmarshal(propsJson, &propsMapOfJson); err != nil {
			log.Fatal(err)
		}
	} else {
		b.WriteString("}\n")
		return b.String()
	}

	propsNamesInOrder, err := keysInOrder(descMap["properties"])
	if err != nil {
		log.Fatal(err)
	}

	// Stores the properties that are required.
	requiredMap := make(map[string]bool)

	if requiredJson, ok := descMap["required"]; ok {
		var required []interface{}
		if err := json.Unmarshal(requiredJson, &required); err != nil {
			log.Fatal(err)
		}
		for _, r := range required {
			requiredMap[r.(string)] = true
		}
	}

	// Some types will have a "body" which should be emitted as a separate type.
	// Since we can't emit a whole new Go type while in the middle of emitting
	// another type, we save it for later and emit it after the current type is
	// done.
	bodyType := ""

	for _, propName := range propsNamesInOrder {
		// The JSON schema is designed for the TypeScript type system, where a
		// subclass can redefine a field in a superclass with a refined type (such
		// as specific values for a field). To ensure we emit Go structs that can
		// be unmarshaled from JSON messages properly, we must limit each field
		// to appear only once in hierarchical types.
		if propName == "type" && (typeName == "Request" || typeName == "Response" || typeName == "Event") {
			continue
		}
		if propName == "command" && typeName != "Request" && typeName != "Response" {
			continue
		}
		if propName == "event" && typeName != "Event" {
			continue
		}
		if propName == "arguments" && typeName == "Request" {
			continue
		}

		var propDesc map[string]interface{}
		if err := json.Unmarshal(propsMapOfJson[propName], &propDesc); err != nil {
			log.Fatal(err)
		}

		if propName == "body" {
			if typeName == "Response" || typeName == "Event" {
				continue
			}

			var bodyTypeName string
			if ref, ok := propDesc["$ref"]; ok {
				bodyTypeName = parseRef(ref)
			} else {
				bodyTypeName = typeName + "Body"
				bodyType = emitToplevelType(bodyTypeName, propsMapOfJson["body"])
			}

			if requiredMap["body"] {
				fmt.Fprintf(&b, "\t%s %s `json:\"body\"`\n", "Body", bodyTypeName)
			} else {
				fmt.Fprintf(&b, "\t%s %s `json:\"body,omitempty\"`\n", "Body", bodyTypeName)
			}
		} else {
			// Go type of this property.
			goType := parsePropertyType(propDesc)

			jsonTag := fmt.Sprintf("`json:\"%s", propName)
			if requiredMap[propName] {
				jsonTag += "\"`"
			} else {
				jsonTag += ",omitempty\"`"
			}

			fmt.Fprintf(&b, "\t%s %s %s\n", goFieldName(propName), goType, jsonTag)
		}
	}

	b.WriteString("}\n")

	if len(bodyType) > 0 {
		b.WriteString("\n")
		b.WriteString(bodyType)
	}

	return b.String()
}

// keysInOrder returns the keys in json object in b, in their original order.
// Based on https://github.com/golang/go/issues/27179#issuecomment-415559968
func keysInOrder(b []byte) ([]string, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if t != json.Delim('{') {
		return nil, errors.New("expected start of object")
	}
	var keys []string
	for {
		t, err := d.Token()
		if err != nil {
			return nil, err
		}
		if t == json.Delim('}') {
			return keys, nil
		}
		keys = append(keys, t.(string))
		if err := skipValue(d); err != nil {
			return nil, err
		}
	}
}

// replaceGoTypename replaces conflicting type names in the JSON schema with
// proper Go type names.
func replaceGoTypename(typeName string) string {
	// Since we have a top-level interface named Message, we replace the DAP
	// message type Message with ErrorMessage.
	if typeName == "Message" {
		return "ErrorMessage"
	}
	return typeName
}

var errEnd = errors.New("invalid end of array or object")

func skipValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	switch t {
	case json.Delim('['), json.Delim('{'):
		for {
			if err := skipValue(d); err != nil {
				if err == errEnd {
					break
				}
				return err
			}
		}
	case json.Delim(']'), json.Delim('}'):
		return errEnd
	}
	return nil
}

const preamble = `// Copyright 2019 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// DO NOT EDIT: This file is auto-generated.
// DAP spec: https://microsoft.github.io/debug-adapter-protocol/specification
// See cmd/gentypes/README.md for additional details.

package dap

// Message is an interface that all DAP message types implement. It's not part
// of the protocol but is used to enforce static typing in Go code.
//
// Note: the DAP type "Message" (which is used in the body of ErrorResponse)
// is renamed to ErrorMessage to avoid collision with this interface.
type Message interface {
	isMessage()
}

`

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	inputFilename := os.Args[1]
	inputData, err := ioutil.ReadFile(inputFilename)
	if err != nil {
		log.Fatal(err)
	}

	var m map[string]json.RawMessage
	if err := json.Unmarshal(inputData, &m); err != nil {
		log.Fatal(err)
	}
	var typeMap map[string]json.RawMessage
	if err := json.Unmarshal(m["definitions"], &typeMap); err != nil {
		log.Fatal(err)
	}

	var b strings.Builder
	b.WriteString(preamble)

	typeNames, err := keysInOrder(m["definitions"])
	if err != nil {
		log.Fatal(err)
	}

	for _, typeName := range typeNames {
		b.WriteString(emitToplevelType(replaceGoTypename(typeName), typeMap[typeName]))
		b.WriteString("\n")
	}

	// For top-level types, emit an empty implementation of isMessage(), to make
	// them implement the Message interface.
	for _, typeName := range typeNames {
		typeName = replaceGoTypename(typeName)
		if strings.HasSuffix(typeName, "Event") || strings.HasSuffix(typeName, "Request") || strings.HasSuffix(typeName, "Response") || typeName == "ProtocolMessage" {
			fmt.Fprintf(&b, "func (%s) isMessage() {}\n", typeName)
		}
	}

	wholeFile := []byte(b.String())
	formatted, err := format.Source(wholeFile)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Print(string(formatted))
}