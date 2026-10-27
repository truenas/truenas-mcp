package tools

import (
	"encoding/json"
	"fmt"
	"math"
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
		n, ok := toInt(v)
		if !ok || n < f.min || n > f.max {
			return nil, fmt.Errorf("%s must be an integer from %d to %d", path, f.min, f.max)
		}
		return n, nil
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

// toInt returns v as an int if it is an integral number in any of the forms
// a decoded JSON argument can take: float64 (encoding/json), json.Number
// (decoders that use UseNumber) or a Go integer. Fractions, strings and
// values outside the int range are rejected.
func toInt(v interface{}) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int8:
		return int(n), true
	case int16:
		return int(n), true
	case int32:
		return int(n), true
	case int64:
		if int64(int(n)) != n {
			return 0, false
		}
		return int(n), true
	case uint:
		if uint64(n) > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case uint8:
		return int(n), true
	case uint16:
		return int(n), true
	case uint32:
		if uint64(n) > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case uint64:
		if n > math.MaxInt32 {
			return 0, false
		}
		return int(n), true
	case float32:
		return floatToInt(float64(n))
	case float64:
		return floatToInt(n)
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return toInt(i)
		}
		f, err := n.Float64()
		if err != nil {
			return 0, false
		}
		return floatToInt(f)
	}
	return 0, false
}

func floatToInt(f float64) (int, bool) {
	if f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, false
	}
	return int(f), true
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
//
// The middleware requires service_type, credential and configuration for
// every update that names a service_type, and service_type for every
// update that enables the service. The one payload without a service_type
// it accepts is a bare disable, {"enable": false}, which keeps the stored
// configuration and turns the service off.
func buildDirectoryServiceUpdatePayload(args map[string]interface{}) (map[string]interface{}, error) {
	payload, err := buildDirectoryServiceUpdatePayloadChecked(args)
	if err != nil {
		// Whichever check failed first, point at the old flat arguments
		// when any of them is present: they are the likely cause.
		top := dsUpdateTopLevelFields()
		if flat := flatArgumentNames(args, top); len(flat) > 0 {
			return nil, fmt.Errorf("%w; the arguments of configure_directory_service changed: %s are no longer top-level arguments. Use service_type + credential + configuration (see the tool description)", err, strings.Join(flat, ", "))
		}
		return nil, err
	}
	return payload, nil
}

// dsUpdateTopLevelFields returns every top-level argument of
// configure_directory_service.
func dsUpdateTopLevelFields() map[string]dsField {
	top := make(map[string]dsField, len(dsTopLevelFields)+3)
	for k, f := range dsTopLevelFields {
		top[k] = f
	}
	top["service_type"] = dsField{kind: dsString}
	top["credential"] = dsField{kind: dsObject}
	top["configuration"] = dsField{kind: dsObject}
	return top
}

func buildDirectoryServiceUpdatePayloadChecked(args map[string]interface{}) (map[string]interface{}, error) {
	// Top-level fields.
	top := dsUpdateTopLevelFields()

	// service_type first: it selects the fields the rest is checked against.
	serviceType := ""
	if v, ok := args["service_type"]; ok {
		str, isStr := v.(string)
		if _, known := dsConfigurationFields[str]; !isStr || !known {
			if !isStr || str == "" {
				return nil, fmt.Errorf("service_type must be one of %s", strings.Join(dsServiceTypes, ", "))
			}
			return nil, fmt.Errorf("service_type must be one of %s, got %q", strings.Join(dsServiceTypes, ", "), str)
		}
		serviceType = str
	}

	payload, err := checkDSFields("arguments", top, args, func(k string) bool { return dsControlArgs[k] })
	if err != nil {
		return nil, err
	}
	enabled, _ := payload["enable"].(bool)

	if serviceType == "" {
		if enabled {
			return nil, fmt.Errorf("service_type is required to enable a directory service: one of %s", strings.Join(dsServiceTypes, ", "))
		}
		// Bare disable: TrueNAS keeps the stored configuration and credential,
		// and rejects a configuration without a service_type.
		for _, k := range []string{"credential", "configuration"} {
			if _, ok := payload[k]; ok {
				return nil, fmt.Errorf("%s needs service_type; to disable the configured directory service omit service_type, credential and configuration and send only enable=false", k)
			}
		}
		return payload, nil
	}
	for _, k := range []string{"credential", "configuration"} {
		if _, ok := payload[k]; !ok {
			return nil, fmt.Errorf("%s is required in arguments when service_type is given", k)
		}
	}
	payload["service_type"] = serviceType
	configFields := dsConfigurationFields[serviceType]

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
	// TrueNAS does not store privileged credentials for a disabled AD or IPA
	// service (common_validation in plugins/directoryservices_/datastore.py).
	if !enabled && credType == "KERBEROS_USER" && serviceType != "LDAP" {
		return nil, fmt.Errorf("KERBEROS_USER credentials may not be stored for a disabled %s service; use a KERBEROS_PRINCIPAL credential, or omit service_type, credential and configuration to disable the configured service", serviceType)
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

// dsLegacyArgumentNames are top-level arguments of the earlier flat form of
// configure_directory_service (type, server, bindname, ...) that are not
// fields of directoryservices.update.
var dsLegacyArgumentNames = []string{
	"type", "server", "servers", "host", "port", "bindname", "bindpw", "binddn", "basedn",
	"netbiosname", "ssl", "domain", "hostname", "username", "password", "certificate",
	"cert_id", "kerberos_principal", "starttls", "validate_certificates",
}

// flatArgumentNames returns, sorted alphabetically, the unknown top-level arguments that
// are names from the old flat form or fields that now belong inside
// credential or configuration.
func flatArgumentNames(args map[string]interface{}, top map[string]dsField) []string {
	known := map[string]bool{}
	for _, n := range dsLegacyArgumentNames {
		known[n] = true
	}
	for _, fields := range dsConfigurationFields {
		for n := range fields {
			known[n] = true
		}
	}
	for _, names := range dsCredentialTypes {
		for _, n := range names {
			known[n] = true
		}
	}
	var out []string
	for k := range args {
		if _, valid := top[k]; !valid && !dsControlArgs[k] && known[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// maskDirectoryServicePayload returns a deep copy of v (a payload or the
// tool arguments) with every secret value replaced by redact.Marker. It
// uses the output redactor's own key matching (any key that names a
// password, secret, token, ...), at every depth, so nested fields such as
// idmap.idmap_domain.ldap_user_dn_password and the same field in
// trusted_domains[] are masked too, and the handler and the redactor that
// processes the tool output agree. The credential_type and the username,
// principal or bind DN of a credential stay visible, and so does
// attribute_maps.passwd, which only holds LDAP attribute names.
func maskDirectoryServicePayload(v map[string]interface{}) map[string]interface{} {
	if masked, ok := redact.Value(v).(map[string]interface{}); ok {
		return masked
	}
	return map[string]interface{}{"payload": redact.Marker}
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
