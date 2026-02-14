package tools

import (
	"fmt"
	"testing"
)

// TestValidateAppName tests app name validation
func TestValidateAppName(t *testing.T) {
	tests := []struct {
		name      string
		appName   string
		wantError bool
	}{
		{"valid lowercase", "plex", false},
		{"valid with numbers", "plex2", false},
		{"valid with hyphens", "my-app", false},
		{"valid with numbers and hyphens", "my-app-123", false},
		{"invalid uppercase", "Plex", true},
		{"invalid starting with hyphen", "-plex", true},
		{"invalid ending with hyphen", "plex-", true},
		{"invalid special characters", "plex_app", true},
		{"invalid empty", "", true},
		{"invalid too long", "a123456789012345678901234567890123456789012", true},
		{"valid max length", "a12345678901234567890123456789012345678", false},
		{"valid single char", "a", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAppName(tt.appName)
			if (err != nil) != tt.wantError {
				t.Errorf("validateAppName(%q) error = %v, wantError %v", tt.appName, err, tt.wantError)
			}
		})
	}
}

// TestValidateStorageVolumes tests storage volume validation
func TestValidateStorageVolumes(t *testing.T) {
	tests := []struct {
		name      string
		volumes   []StorageVolume
		wantError bool
	}{
		{
			"valid single volume",
			[]StorageVolume{{Name: "config", Path: "/mnt/tank/apps/plex/config"}},
			false,
		},
		{
			"valid multiple volumes",
			[]StorageVolume{
				{Name: "config", Path: "/mnt/tank/apps/plex/config"},
				{Name: "data", Path: "/mnt/tank/apps/plex/data"},
			},
			false,
		},
		{
			"invalid empty list",
			[]StorageVolume{},
			true,
		},
		{
			"invalid duplicate names",
			[]StorageVolume{
				{Name: "config", Path: "/mnt/tank/apps/plex/config"},
				{Name: "config", Path: "/mnt/tank/apps/plex/config2"},
			},
			true,
		},
		{
			"invalid duplicate paths",
			[]StorageVolume{
				{Name: "config", Path: "/mnt/tank/apps/plex/config"},
				{Name: "data", Path: "/mnt/tank/apps/plex/config"},
			},
			true,
		},
		{
			"invalid path without /mnt/",
			[]StorageVolume{{Name: "config", Path: "/tank/apps/plex/config"}},
			true,
		},
		{
			"invalid empty name",
			[]StorageVolume{{Name: "", Path: "/mnt/tank/apps/plex/config"}},
			true,
		},
		{
			"invalid empty path",
			[]StorageVolume{{Name: "config", Path: ""}},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateStorageVolumes(tt.volumes)
			if (err != nil) != tt.wantError {
				t.Errorf("validateStorageVolumes() error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

// TestBuildPersistenceConfig tests persistence config building
func TestBuildPersistenceConfig(t *testing.T) {
	volumes := []StorageVolume{
		{Name: "config", Path: "/mnt/tank/apps/plex/config"},
		{Name: "data", Path: "/mnt/tank/apps/plex/data"},
	}

	config := buildPersistenceConfig(volumes)

	// Check config structure
	if len(config) != 2 {
		t.Errorf("Expected 2 volumes in config, got %d", len(config))
	}

	// Check config volume
	configVol, ok := config["config"].(map[string]interface{})
	if !ok {
		t.Fatal("config volume not found or wrong type")
	}
	if configVol["type"] != "host-path" {
		t.Errorf("Expected type 'host-path', got %v", configVol["type"])
	}
	if configVol["hostPath"] != "/mnt/tank/apps/plex/config" {
		t.Errorf("Expected hostPath '/mnt/tank/apps/plex/config', got %v", configVol["hostPath"])
	}

	// Check data volume
	dataVol, ok := config["data"].(map[string]interface{})
	if !ok {
		t.Fatal("data volume not found or wrong type")
	}
	if dataVol["type"] != "host-path" {
		t.Errorf("Expected type 'host-path', got %v", dataVol["type"])
	}
	if dataVol["hostPath"] != "/mnt/tank/apps/plex/data" {
		t.Errorf("Expected hostPath '/mnt/tank/apps/plex/data', got %v", dataVol["hostPath"])
	}
}

// TestParseStoragePath tests storage path parsing
func TestParseStoragePath(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		wantPool    string
		wantDataset string
		wantError   bool
	}{
		{
			"valid path",
			"/mnt/tank/apps/plex/config",
			"tank",
			"tank/apps/plex/config",
			false,
		},
		{
			"valid short path",
			"/mnt/tank/data",
			"tank",
			"tank/data",
			false,
		},
		{
			"invalid without /mnt/",
			"/tank/data",
			"",
			"",
			true,
		},
		{
			"invalid too short",
			"/mnt/tank",
			"",
			"",
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool, dataset, err := parseStoragePath(tt.path)
			if (err != nil) != tt.wantError {
				t.Errorf("parseStoragePath(%q) error = %v, wantError %v", tt.path, err, tt.wantError)
			}
			if !tt.wantError {
				if pool != tt.wantPool {
					t.Errorf("parseStoragePath(%q) pool = %q, want %q", tt.path, pool, tt.wantPool)
				}
				if dataset != tt.wantDataset {
					t.Errorf("parseStoragePath(%q) dataset = %q, want %q", tt.path, dataset, tt.wantDataset)
				}
			}
		})
	}
}

// TestParseAppREADMEForStorageHints tests storage hint extraction
func TestParseAppREADMEForStorageHints(t *testing.T) {
	tests := []struct {
		name         string
		readme       string
		wantHints    int
		shouldFind   []string
		shouldntFind []string
	}{
		{
			"readme with config and data",
			"This app requires a config volume and a data storage directory.",
			2,
			[]string{"config", "data"},
			[]string{"media"},
		},
		{
			"readme with media volume",
			"Mount your media volume at /media for the app to access files.",
			1,
			[]string{"media"},
			[]string{"config"},
		},
		{
			"readme with postgres volume",
			"The app uses a postgres volume for database persistence.",
			1,
			[]string{"postgres"},
			[]string{"mysql"},
		},
		{
			"readme without storage hints",
			"This is a simple app with no storage requirements mentioned.",
			0,
			[]string{},
			[]string{"config", "data"},
		},
		{
			"readme with logs path",
			"Application logs will be written to the logs directory.",
			1,
			[]string{"logs"},
			[]string{"data"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hints := parseAppREADMEForStorageHints(tt.readme)

			if len(hints) != tt.wantHints {
				t.Errorf("parseAppREADMEForStorageHints() returned %d hints, want %d", len(hints), tt.wantHints)
			}

			hintMap := make(map[string]bool)
			for _, hint := range hints {
				hintMap[hint] = true
			}

			for _, expected := range tt.shouldFind {
				if !hintMap[expected] {
					t.Errorf("Expected to find hint %q but didn't", expected)
				}
			}

			for _, unexpected := range tt.shouldntFind {
				if hintMap[unexpected] {
					t.Errorf("Did not expect to find hint %q but did", unexpected)
				}
			}
		})
	}
}

// TestExtractStorageVolumes tests extraction of storage volumes from args
func TestExtractStorageVolumes(t *testing.T) {
	tests := []struct {
		name      string
		args      map[string]interface{}
		wantLen   int
		wantError bool
	}{
		{
			"valid single volume",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name": "config",
						"path": "/mnt/tank/apps/plex/config",
					},
				},
			},
			1,
			false,
		},
		{
			"valid multiple volumes",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name": "config",
						"path": "/mnt/tank/apps/plex/config",
					},
					map[string]interface{}{
						"name": "data",
						"path": "/mnt/tank/apps/plex/data",
					},
				},
			},
			2,
			false,
		},
		{
			"missing storage_volumes",
			map[string]interface{}{},
			0,
			true,
		},
		{
			"empty storage_volumes",
			map[string]interface{}{
				"storage_volumes": []interface{}{},
			},
			0,
			true,
		},
		{
			"invalid storage_volumes type",
			map[string]interface{}{
				"storage_volumes": "not an array",
			},
			0,
			true,
		},
		{
			"missing name field",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"path": "/mnt/tank/apps/plex/config",
					},
				},
			},
			0,
			true,
		},
		{
			"missing path field",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name": "config",
					},
				},
			},
			0,
			true,
		},
		{
			"user mistake: using host_path instead of path",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name":      "config",
						"host_path": "/mnt/tank/apps/plex/config",
					},
				},
			},
			0,
			true,
		},
		{
			"user mistake: using host_path and mount_path",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name":       "config",
						"type":       "host_path",
						"host_path":  "/mnt/tank/apps/plex/config",
						"mount_path": "/config",
					},
				},
			},
			0,
			true,
		},
		{
			"user mistake: extra type field (but has correct fields)",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name": "config",
						"type": "host_path",
						"path": "/mnt/tank/apps/plex/config",
					},
				},
			},
			1, // Should work despite extra field
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			volumes, err := extractStorageVolumes(tt.args)
			if (err != nil) != tt.wantError {
				t.Errorf("extractStorageVolumes() error = %v, wantError %v", err, tt.wantError)
			}
			if !tt.wantError && len(volumes) != tt.wantLen {
				t.Errorf("extractStorageVolumes() returned %d volumes, want %d", len(volumes), tt.wantLen)
			}
		})
	}
}

// TestExtractStorageVolumesErrorMessages tests that error messages are helpful
func TestExtractStorageVolumesErrorMessages(t *testing.T) {
	tests := []struct {
		name            string
		args            map[string]interface{}
		wantErrorSubstr []string
	}{
		{
			"missing name shows example",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"path": "/mnt/tank/apps/plex/config",
					},
				},
			},
			[]string{"missing required 'name' field", "Example:", `"name"`, `"path"`},
		},
		{
			"wrong field name (host_path) suggests correction",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name":      "config",
						"host_path": "/mnt/tank/apps/plex/config",
					},
				},
			},
			[]string{"missing required 'path' field", "Found 'host_path'", "did you mean 'path'?", "Example:"},
		},
		{
			"mount_path field shows it's not needed",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name":       "config",
						"mount_path": "/config",
					},
				},
			},
			[]string{"missing required 'path' field", "Found 'mount_path'", "not needed", "Example:"},
		},
		{
			"type field shows it's not needed",
			map[string]interface{}{
				"storage_volumes": []interface{}{
					map[string]interface{}{
						"name": "config",
						"type": "host_path",
					},
				},
			},
			[]string{"missing required 'path' field", "Found 'type'", "not needed", "host-path volumes", "Example:"},
		},
		{
			"missing storage_volumes shows example",
			map[string]interface{}{},
			[]string{"storage_volumes is required", "Expected:", "Example:", `"name"`, `"path"`},
		},
		{
			"storage_volumes not array shows example",
			map[string]interface{}{
				"storage_volumes": "not an array",
			},
			[]string{"storage_volumes must be an array", "Expected:", "Example:", `"name"`, `"path"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := extractStorageVolumes(tt.args)
			if err == nil {
				t.Fatal("Expected error but got none")
			}

			errMsg := err.Error()
			for _, substr := range tt.wantErrorSubstr {
				if !containsIgnoreCase(errMsg, substr) {
					t.Errorf("Error message doesn't contain %q.\nGot: %s", substr, errMsg)
				}
			}
		})
	}
}

// Helper function for case-insensitive substring search
func containsIgnoreCase(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr ||
		len(substr) == 0 ||
		indexIgnoreCase(s, substr) >= 0)
}

func indexIgnoreCase(s, substr string) int {
	s = toLower(s)
	substr = toLower(substr)
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

func toLower(s string) string {
	b := make([]byte, len(s))
	for i := range s {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

// ============================================================================
// Schema-Driven Configuration Tests
// ============================================================================

// TestExtractAppSchema tests schema extraction from app details
func TestExtractAppSchema(t *testing.T) {
	tests := []struct {
		name       string
		appDetails map[string]interface{}
		wantNil    bool
	}{
		{
			"valid schema",
			map[string]interface{}{
				"latest_version": "1.0.0",
				"versions": map[string]interface{}{
					"1.0.0": map[string]interface{}{
						"schema": map[string]interface{}{
							"groups": []interface{}{
								map[string]interface{}{"name": "App Configuration"},
								map[string]interface{}{"name": "Storage Configuration"},
							},
							"questions": []interface{}{
								map[string]interface{}{"variable": "TZ", "group": "App Configuration"},
							},
						},
					},
				},
			},
			false,
		},
		{
			"missing versions",
			map[string]interface{}{
				"latest_version": "1.0.0",
			},
			true,
		},
		{
			"missing latest_version",
			map[string]interface{}{
				"versions": map[string]interface{}{},
			},
			true,
		},
		{
			"missing version data",
			map[string]interface{}{
				"latest_version": "1.0.0",
				"versions":       map[string]interface{}{},
			},
			true,
		},
		{
			"missing schema",
			map[string]interface{}{
				"latest_version": "1.0.0",
				"versions": map[string]interface{}{
					"1.0.0": map[string]interface{}{},
				},
			},
			true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			schema := extractAppSchema(tt.appDetails)
			if (schema == nil) != tt.wantNil {
				t.Errorf("extractAppSchema() returned nil=%v, want nil=%v", schema == nil, tt.wantNil)
			}
		})
	}
}

// TestFormatSchemaForWizard tests schema formatting for LLM
func TestFormatSchemaForWizard(t *testing.T) {
	schema := map[string]interface{}{
		"groups": []interface{}{
			map[string]interface{}{"name": "App Configuration", "description": "Basic settings"},
			map[string]interface{}{"name": "Storage Configuration", "description": "Storage volumes"},
			map[string]interface{}{"name": "Network Configuration", "description": "Port settings"},
		},
		"questions": []interface{}{
			map[string]interface{}{"variable": "TZ", "group": "App Configuration"},
			map[string]interface{}{"variable": "storage", "group": "Storage Configuration"},
			map[string]interface{}{"variable": "network", "group": "Network Configuration"},
			map[string]interface{}{"variable": "additional_storage", "group": "Storage Configuration"},
		},
	}

	formatted := formatSchemaForWizard(schema)

	if formatted == nil {
		t.Fatal("formatSchemaForWizard() returned nil")
	}

	// Check groups
	groups, ok := formatted["groups"].([]map[string]interface{})
	if !ok {
		t.Fatal("groups field missing or wrong type")
	}
	if len(groups) != 3 {
		t.Errorf("Expected 3 groups, got %d", len(groups))
	}

	// Check questions_by_group
	questionsByGroup, ok := formatted["questions_by_group"].(map[string][]map[string]interface{})
	if !ok {
		t.Fatal("questions_by_group field missing or wrong type")
	}
	if len(questionsByGroup["App Configuration"]) != 1 {
		t.Errorf("Expected 1 question in App Configuration, got %d", len(questionsByGroup["App Configuration"]))
	}
	if len(questionsByGroup["Storage Configuration"]) != 2 {
		t.Errorf("Expected 2 questions in Storage Configuration, got %d", len(questionsByGroup["Storage Configuration"]))
	}

	// Check group_count
	groupCount, ok := formatted["group_count"].(int)
	if !ok || groupCount != 3 {
		t.Errorf("Expected group_count=3, got %v", groupCount)
	}

	// Check note
	note, ok := formatted["note"].(string)
	if !ok || note == "" {
		t.Error("Expected non-empty note field")
	}
}

// TestSummarizeQuestion tests question summarization with large enums
func TestSummarizeQuestion(t *testing.T) {
	// Test large enum summarization (like timezone with 600+ options)
	largeEnum := make([]interface{}, 650)
	for i := 0; i < 650; i++ {
		largeEnum[i] = fmt.Sprintf("Timezone_%d", i)
	}

	question := map[string]interface{}{
		"variable":    "TZ",
		"label":       "Timezone",
		"description": "Select timezone",
		"group":       "App Configuration",
		"schema": map[string]interface{}{
			"type":     "string",
			"required": true,
			"default":  "Etc/UTC",
			"enum":     largeEnum,
		},
	}

	summarized := summarizeQuestion(question)

	// Check core fields preserved
	if summarized["variable"] != "TZ" {
		t.Errorf("Expected variable=TZ, got %v", summarized["variable"])
	}
	if summarized["label"] != "Timezone" {
		t.Errorf("Expected label=Timezone, got %v", summarized["label"])
	}

	// Check schema info
	schemaInfo, ok := summarized["schema"].(map[string]interface{})
	if !ok {
		t.Fatal("schema field missing or wrong type")
	}

	// Check enum summarization
	enumInfo, ok := schemaInfo["enum"].(map[string]interface{})
	if !ok {
		t.Fatal("enum should be summarized as map for large enums")
	}

	count, ok := enumInfo["count"].(int)
	if !ok || count != 650 {
		t.Errorf("Expected enum count=650, got %v", count)
	}

	examples, ok := enumInfo["examples"].([]interface{})
	if !ok || len(examples) != 3 {
		t.Errorf("Expected 3 examples, got %v", len(examples))
	}

	// Test small enum (should not be summarized)
	smallEnum := []interface{}{"option1", "option2", "option3"}
	question2 := map[string]interface{}{
		"variable": "small_enum",
		"schema": map[string]interface{}{
			"type": "string",
			"enum": smallEnum,
		},
	}

	summarized2 := summarizeQuestion(question2)
	schemaInfo2 := summarized2["schema"].(map[string]interface{})
	enumArray, ok := schemaInfo2["enum"].([]interface{})
	if !ok || len(enumArray) != 3 {
		t.Error("Small enum should be preserved as array")
	}
}

// TestEnforceHostPathStorage tests recursive storage validation
func TestEnforceHostPathStorage(t *testing.T) {
	tests := []struct {
		name      string
		values    map[string]interface{}
		wantError bool
		errorText string
	}{
		{
			"valid host_path storage",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path":       "/mnt/tank/apps/jellyfin/config",
							"acl_enable": false,
						},
					},
				},
			},
			false,
			"",
		},
		{
			"valid nested host_path storage",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path": "/mnt/tank/apps/jellyfin/config",
						},
					},
					"cache": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path": "/mnt/tank/apps/jellyfin/cache",
						},
					},
				},
			},
			false,
			"",
		},
		{
			"invalid ix_volume detected",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "ix_volume",
						"ix_volume_config": map[string]interface{}{
							"dataset_name": "config",
						},
					},
				},
			},
			true,
			"ix_volume not allowed",
		},
		{
			"invalid ix_volume_config detected",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "host_path",
						"ix_volume_config": map[string]interface{}{
							"dataset_name": "config",
						},
					},
				},
			},
			true,
			"ix_volume_config not allowed",
		},
		{
			"invalid path without /mnt/",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path": "/tank/apps/jellyfin/config",
						},
					},
				},
			},
			true,
			"must start with /mnt/",
		},
		{
			"deeply nested ix_volume detected",
			map[string]interface{}{
				"jellyfin": map[string]interface{}{
					"storage": map[string]interface{}{
						"additional_storage": []interface{}{
							map[string]interface{}{
								"type": "ix_volume",
								"ix_volume_config": map[string]interface{}{
									"dataset_name": "extra",
								},
							},
						},
					},
				},
			},
			true,
			"ix_volume not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := enforceHostPathStorage(tt.values)
			if (err != nil) != tt.wantError {
				t.Errorf("enforceHostPathStorage() error = %v, wantError %v", err, tt.wantError)
			}
			if tt.wantError && err != nil {
				if !containsIgnoreCase(err.Error(), tt.errorText) {
					t.Errorf("Expected error containing %q, got %q", tt.errorText, err.Error())
				}
			}
		})
	}
}

// TestExtractStoragePathsFromValues tests path extraction from values
func TestExtractStoragePathsFromValues(t *testing.T) {
	tests := []struct {
		name      string
		values    map[string]interface{}
		wantPaths []string
	}{
		{
			"single storage path",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path": "/mnt/tank/apps/jellyfin/config",
						},
					},
				},
			},
			[]string{"/mnt/tank/apps/jellyfin/config"},
		},
		{
			"multiple storage paths",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"config": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path": "/mnt/tank/apps/jellyfin/config",
						},
					},
					"cache": map[string]interface{}{
						"type": "host_path",
						"host_path_config": map[string]interface{}{
							"path": "/mnt/tank/apps/jellyfin/cache",
						},
					},
				},
			},
			[]string{"/mnt/tank/apps/jellyfin/config", "/mnt/tank/apps/jellyfin/cache"},
		},
		{
			"paths in additional_storage array",
			map[string]interface{}{
				"storage": map[string]interface{}{
					"additional_storage": []interface{}{
						map[string]interface{}{
							"type": "host_path",
							"host_path_config": map[string]interface{}{
								"path": "/mnt/tank/media",
							},
						},
						map[string]interface{}{
							"type": "host_path",
							"host_path_config": map[string]interface{}{
								"path": "/mnt/tank/downloads",
							},
						},
					},
				},
			},
			[]string{"/mnt/tank/media", "/mnt/tank/downloads"},
		},
		{
			"no storage paths",
			map[string]interface{}{
				"TZ": "America/New_York",
				"network": map[string]interface{}{
					"web_port": 8080,
				},
			},
			[]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths := extractStoragePathsFromValues(tt.values)
			if len(paths) != len(tt.wantPaths) {
				t.Errorf("extractStoragePathsFromValues() returned %d paths, want %d", len(paths), len(tt.wantPaths))
			}

			pathSet := make(map[string]bool)
			for _, p := range paths {
				pathSet[p] = true
			}

			for _, wantPath := range tt.wantPaths {
				if !pathSet[wantPath] {
					t.Errorf("Expected path %q not found in result", wantPath)
				}
			}
		})
	}
}

// TestGenerateWizardGuidance tests wizard guidance generation
func TestGenerateWizardGuidance(t *testing.T) {
	schema := map[string]interface{}{
		"groups": []interface{}{
			map[string]interface{}{"name": "App Configuration"},
			map[string]interface{}{"name": "Storage Configuration"},
		},
	}

	guidance := generateWizardGuidance(schema)

	if guidance == nil {
		t.Fatal("generateWizardGuidance() returned nil")
	}

	// Check workflow
	workflow, ok := guidance["workflow"].(string)
	if !ok || workflow == "" {
		t.Error("Expected non-empty workflow field")
	}

	// Check steps
	steps, ok := guidance["steps"].([]string)
	if !ok || len(steps) == 0 {
		t.Error("Expected non-empty steps array")
	}
	if len(steps) != 10 {
		t.Errorf("Expected 10 steps, got %d", len(steps))
	}

	// Check common_patterns
	patterns, ok := guidance["common_patterns"].(map[string]interface{})
	if !ok || len(patterns) == 0 {
		t.Error("Expected non-empty common_patterns")
	}

	// Verify critical patterns exist
	criticalPatterns := []string{"timezone", "run_as", "storage_type", "storage_paths", "port_bind_mode", "resources"}

	// Check storage_workflow
	storageWorkflow, ok := guidance["storage_workflow"].(map[string]interface{})
	if !ok || len(storageWorkflow) == 0 {
		t.Error("Expected non-empty storage_workflow")
	}
	for _, pattern := range criticalPatterns {
		if _, exists := patterns[pattern]; !exists {
			t.Errorf("Expected pattern %q not found in common_patterns", pattern)
		}
	}
}
