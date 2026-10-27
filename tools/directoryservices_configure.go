package tools

import (
	"fmt"
	"sort"
	"strings"

	"github.com/truenas/truenas-mcp/redact"
)

// This file builds the payload for TrueNAS's directoryservices.update
// (TrueNAS 25.10 and later). The field names, required fields and allowed
// values follow the middleware's DirectoryServicesUpdateArgs model
// (api/v25_10_0/directory_services.py, unchanged through v27_0_0). The
// configure_directory_service tool's input schema (registry.go) uses the
// same names, so the arguments a client sends are the payload fields.

type dsFieldKind int

const (
	dsString     dsFieldKind = iota // non-empty string
	dsBool                          // boolean
	dsStringList                    // non-empty array of non-empty strings
	dsObject                        // object, passed through to TrueNAS to validate
	dsObjectList                    // array of objects, passed through to TrueNAS to validate
	dsInt                           // integer within [min, max]
)

type dsField struct {
	kind     dsFieldKind
	required bool
	enum     []string // allowed values for a dsString, when set
	min, max int      // bounds of a dsInt
}

// dsServiceTypes are the values of service_type the tool accepts.
var dsServiceTypes = []string{"ACTIVEDIRECTORY", "IPA", "LDAP"}

// dsConfigurationFields lists the configuration fields of each service type.
// TrueNAS rejects unknown fields, so the tool does too.
var dsConfigurationFields = map[string]map[string]dsField{
	"ACTIVEDIRECTORY": {
		"hostname":               {kind: dsString, required: true},
		"domain":                 {kind: dsString, required: true},
		"idmap":                  {kind: dsObject},
		"site":                   {kind: dsString},
		"computer_account_ou":    {kind: dsString},
		"use_default_domain":     {kind: dsBool},
		"enable_trusted_domains": {kind: dsBool},
		"trusted_domains":        {kind: dsObjectList},
	},
	"IPA": {
		"target_server":         {kind: dsString, required: true},
		"hostname":              {kind: dsString, required: true},
		"domain":                {kind: dsString, required: true},
		"basedn":                {kind: dsString, required: true},
		"smb_domain":            {kind: dsObject},
		"validate_certificates": {kind: dsBool},
	},
	"LDAP": {
		"server_urls":           {kind: dsStringList, required: true},
		"basedn":                {kind: dsString, required: true},
		"starttls":              {kind: dsBool},
		"validate_certificates": {kind: dsBool},
		"schema":                {kind: dsString, enum: []string{"RFC2307", "RFC2307BIS"}},
		"search_bases":          {kind: dsObject},
		"attribute_maps":        {kind: dsObject},
		"auxiliary_parameters":  {kind: dsString},
	},
}

// dsCredentialTypes lists the credential types and their fields (all
// required, all non-empty strings) in the order TrueNAS documents them.
var dsCredentialTypes = map[string][]string{
	"KERBEROS_USER":      {"username", "password"},
	"KERBEROS_PRINCIPAL": {"principal"},
	"LDAP_PLAIN":         {"binddn", "bindpw"},
	"LDAP_ANONYMOUS":     {},
	"LDAP_MTLS":          {"client_certificate"},
}

// dsSecretFields hold values that must never be echoed back.
var dsSecretFields = map[string]bool{"password": true, "bindpw": true}

// dsTopLevelFields are the update fields besides service_type, credential
// and configuration, which have their own handling.
var dsTopLevelFields = map[string]dsField{
	"enable":               {kind: dsBool, required: true},
	"enable_account_cache": {kind: dsBool},
	"enable_dns_updates":   {kind: dsBool},
	"kerberos_realm":       {kind: dsString},
	"force":                {kind: dsBool},
	"timeout":              {kind: dsInt, min: 5, max: 60},
}

// dsControlArgs are tool-level arguments that never reach TrueNAS.
var dsControlArgs = map[string]bool{"dry_run": true, "confirm": true}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// checkDSValue validates one value against its field spec and returns the
// value to send.
func checkDSValue(path string, f dsField, v interface{}) (interface{}, error) {
	switch f.kind {
	case dsString:
		str, ok := v.(string)
		if !ok || str == "" {
			return nil, fmt.Errorf("%s must be a non-empty string", path)
		}
		if len(f.enum) > 0 {
			for _, e := range f.enum {
				if str == e {
					return str, nil
				}
			}
			return nil, fmt.Errorf("%s must be one of %s", path, strings.Join(f.enum, ", "))
		}
		return str, nil
	case dsBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("%s must be a boolean", path)
		}
		return b, nil
	case dsInt:
		n, ok := v.(float64)
		if !ok || n != float64(int(n)) || int(n) < f.min || int(n) > f.max {
			return nil, fmt.Errorf("%s must be an integer from %d to %d", path, f.min, f.max)
		}
		return int(n), nil
	case dsStringList:
		list, ok := v.([]interface{})
		if !ok || len(list) == 0 {
			return nil, fmt.Errorf("%s must be a non-empty array of strings", path)
		}
		for i, e := range list {
			if str, ok := e.(string); !ok || str == "" {
				return nil, fmt.Errorf("%s[%d] must be a non-empty string", path, i)
			}
		}
		return list, nil
	case dsObject:
		obj, ok := v.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("%s must be an object", path)
		}
		return obj, nil
	case dsObjectList:
		list, ok := v.([]interface{})
		if !ok {
			return nil, fmt.Errorf("%s must be an array of objects", path)
		}
		for i, e := range list {
			if _, ok := e.(map[string]interface{}); !ok {
				return nil, fmt.Errorf("%s[%d] must be an object", path, i)
			}
		}
		return list, nil
	}
	return nil, fmt.Errorf("%s: unsupported field kind", path)
}

// checkDSFields validates obj against fields: unknown keys are rejected,
// required keys must be present and every value must have the right type.
func checkDSFields(path string, fields map[string]dsField, obj map[string]interface{}, skip func(string) bool) (map[string]interface{}, error) {
	out := make(map[string]interface{}, len(obj))
	for _, k := range sortedKeys(obj) {
		if skip != nil && skip(k) {
			continue
		}
		f, ok := fields[k]
		if !ok {
			return nil, fmt.Errorf("unknown field %q in %s; allowed fields: %s", k, path, strings.Join(sortedKeys(fields), ", "))
		}
		v, err := checkDSValue(path+"."+k, f, obj[k])
		if err != nil {
			return nil, err
		}
		out[k] = v
	}
	for _, k := range sortedKeys(fields) {
		if _, ok := out[k]; fields[k].required && !ok {
			return nil, fmt.Errorf("%s is required in %s", k, path)
		}
	}
	return out, nil
}

// buildDirectoryServiceUpdatePayload validates the tool arguments and
// returns the single argument of directoryservices.update. Only fields the
// caller supplied are included: TrueNAS keeps the current value of anything
// omitted. The dry run shows this exact value (secrets masked).
func buildDirectoryServiceUpdatePayload(args map[string]interface{}) (map[string]interface{}, error) {
	serviceType, ok := args["service_type"].(string)
	if !ok || serviceType == "" {
		return nil, fmt.Errorf("service_type is required: one of %s", strings.Join(dsServiceTypes, ", "))
	}
	configFields, known := dsConfigurationFields[serviceType]
	if !known {
		return nil, fmt.Errorf("service_type must be one of %s, got %q", strings.Join(dsServiceTypes, ", "), serviceType)
	}

	// Top-level fields.
	top := make(map[string]dsField, len(dsTopLevelFields)+4)
	for k, f := range dsTopLevelFields {
		top[k] = f
	}
	top["service_type"] = dsField{kind: dsString}
	top["credential"] = dsField{kind: dsObject, required: true}
	top["configuration"] = dsField{kind: dsObject, required: true}
	payload, err := checkDSFields("arguments", top, args, func(k string) bool { return dsControlArgs[k] })
	if err != nil {
		return nil, err
	}
	payload["service_type"] = serviceType

	// credential
	credIn, _ := payload["credential"].(map[string]interface{})
	credType, _ := credIn["credential_type"].(string)
	credFieldNames, ok := dsCredentialTypes[credType]
	if !ok {
		return nil, fmt.Errorf("credential.credential_type is required: one of %s", strings.Join(sortedKeys(dsCredentialTypes), ", "))
	}
	credFields := map[string]dsField{"credential_type": {kind: dsString, required: true}}
	for _, name := range credFieldNames {
		credFields[name] = dsField{kind: dsString, required: true}
	}
	cred, err := checkDSFields("credential ("+credType+")", credFields, credIn, nil)
	if err != nil {
		return nil, err
	}
	payload["credential"] = cred

	isKerberos := strings.HasPrefix(credType, "KERBEROS")
	if serviceType != "LDAP" && strings.HasPrefix(credType, "LDAP") {
		return nil, fmt.Errorf("credential type %s is only supported for service_type LDAP; use KERBEROS_USER or KERBEROS_PRINCIPAL for %s", credType, serviceType)
	}
	if serviceType == "LDAP" && isKerberos {
		if _, ok := payload["kerberos_realm"]; !ok {
			return nil, fmt.Errorf("kerberos_realm is required to use %s credentials with service_type LDAP", credType)
		}
	}

	// configuration
	confIn, _ := payload["configuration"].(map[string]interface{})
	conf, err := checkDSFields("configuration ("+serviceType+")", configFields, confIn, nil)
	if err != nil {
		return nil, err
	}
	payload["configuration"] = conf

	return payload, nil
}

// maskDirectoryServicePayload returns a deep copy of v (a payload or the
// tool arguments) with password and bindpw values replaced by redact.Marker.
func maskDirectoryServicePayload(v map[string]interface{}) map[string]interface{} {
	return maskDSValue(v).(map[string]interface{})
}

func maskDSValue(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, e := range t {
			if dsSecretFields[k] {
				out[k] = redact.Marker
			} else {
				out[k] = maskDSValue(e)
			}
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, e := range t {
			out[i] = maskDSValue(e)
		}
		return out
	}
	return v
}

// directoryservices.leave (TrueNAS 25.10 and later) takes one argument,
// {"credential": <KERBEROS_USER credential>}, and only supports
// ACTIVEDIRECTORY and IPA (DirectoryServicesLeaveArgs in
// api/v25_10_0/directory_services.py, unchanged through v27_0_0; the
// middleware's leave() raises for LDAP). It is a job.
const dsLeaveCredentialType = "KERBEROS_USER"

// dsLeaveServiceTypes are the service types that can be left.
var dsLeaveServiceTypes = []string{"ACTIVEDIRECTORY", "IPA"}

// buildDirectoryServiceLeavePayload validates the leave_directory_service
// arguments and returns the single argument of directoryservices.leave. The
// dry run shows this exact value (password masked).
func buildDirectoryServiceLeavePayload(args map[string]interface{}) (map[string]interface{}, error) {
	top := map[string]dsField{"credential": {kind: dsObject, required: true}}
	checked, err := checkDSFields("arguments", top, args, func(k string) bool { return dsControlArgs[k] })
	if err != nil {
		return nil, err
	}
	credIn, _ := checked["credential"].(map[string]interface{})
	if t, _ := credIn["credential_type"].(string); t != dsLeaveCredentialType {
		return nil, fmt.Errorf("credential.credential_type is required and must be %s, the only credential directoryservices.leave accepts", dsLeaveCredentialType)
	}
	credFields := map[string]dsField{"credential_type": {kind: dsString, required: true}}
	for _, name := range dsCredentialTypes[dsLeaveCredentialType] {
		credFields[name] = dsField{kind: dsString, required: true}
	}
	cred, err := checkDSFields("credential ("+dsLeaveCredentialType+")", credFields, credIn, nil)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"credential": cred}, nil
}

// checkDirectoryServiceCanLeave applies the middleware's own preconditions
// for leaving to the current status.
func checkDirectoryServiceCanLeave(status *DirectoryServiceStatus) error {
	if status.Type == "none" {
		return fmt.Errorf("no directory service is currently configured")
	}
	if !status.Enabled {
		return fmt.Errorf("%s is configured but not enabled", status.Type)
	}
	for _, t := range dsLeaveServiceTypes {
		if status.Type == t {
			return nil
		}
	}
	return fmt.Errorf("%s does not support leave operations (only %s do); use configure_directory_service with enable=false to disable it", status.Type, strings.Join(dsLeaveServiceTypes, " and "))
}
