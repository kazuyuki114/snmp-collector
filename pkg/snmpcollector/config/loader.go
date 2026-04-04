// Package config provides YAML configuration loading for the SNMP Collector.
//
// It reads five directory trees (driven by environment variables) and produces
// a LoadedConfig value that is used by the rest of the application.
//
//	INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH     → Devices map
//	INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH → DeviceGroups map
//	INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH → ObjectGroups map
//	INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH     → ObjectDefs map
//	PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH   → EnumRegistry
package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"snmp/snmp-collector/models"
	"snmp/snmp-collector/producer/metrics"
)

// ─────────────────────────────────────────────────────────────────────────────
// Paths
// ─────────────────────────────────────────────────────────────────────────────

// Paths holds the directory locations for every configuration tree.
type Paths struct {
	Devices      string // INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH
	DeviceGroups string // INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH
	ObjectGroups string // INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH
	Objects      string // INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH
	Enums        string // PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH
}

// PathsFromEnv reads each path from its environment variable, falling back to
// the documented default when the variable is unset or empty.
func PathsFromEnv() Paths {
	return Paths{
		Devices:      envOr("INPUT_SNMP_DEVICE_DEFINITIONS_DIRECTORY_PATH", "/etc/snmp_collector/snmp/devices"),
		DeviceGroups: envOr("INPUT_SNMP_DEVICE_GROUP_DEFINITIONS_DIRECTORY_PATH", "/etc/snmp_collector/snmp/device_groups"),
		ObjectGroups: envOr("INPUT_SNMP_OBJECT_GROUP_DEFINITIONS_DIRECTORY_PATH", "/etc/snmp_collector/snmp/object_groups"),
		Objects:      envOr("INPUT_SNMP_OBJECT_DEFINITIONS_DIRECTORY_PATH", "/etc/snmp_collector/snmp/objects"),
		Enums:        envOr("PROCESSOR_SNMP_ENUM_DEFINITIONS_DIRECTORY_PATH", "/etc/snmp_collector/snmp/enums"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ─────────────────────────────────────────────────────────────────────────────
// LoadedConfig
// ─────────────────────────────────────────────────────────────────────────────

// LoadedConfig is the fully parsed representation of all configuration trees.
type LoadedConfig struct {
	// Devices maps hostname → resolved DeviceConfig (hard-coded fallbacks applied).
	Devices map[string]DeviceConfig

	// DeviceGroups maps group name → DeviceGroup.
	DeviceGroups map[string]DeviceGroup

	// ObjectGroups maps group name → ObjectGroup.
	ObjectGroups map[string]ObjectGroup

	// ObjectDefs maps object key (e.g. "IF-MIB::ifEntry") → ObjectDefinition.
	ObjectDefs map[string]models.ObjectDefinition

	// Enums is the populated EnumRegistry ready for the producer.
	// nil when the enums directory is empty or does not exist.
	Enums *metrics.EnumRegistry
}

// ─────────────────────────────────────────────────────────────────────────────
// Load
// ─────────────────────────────────────────────────────────────────────────────

// Load reads all configuration directories specified by paths and returns a
// fully resolved LoadedConfig. Errors from individual files are accumulated and
// returned together so that operators see all problems at once.
//
// If a directory does not exist, that section is skipped silently (the
// corresponding map will be empty / nil). This allows partial deployments.
func Load(paths Paths, logger *slog.Logger) (*LoadedConfig, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(noopWriter{}, nil))
	}

	var errs []string

	// 1. Devices ——————————————————————————————————————————————————————————————
	devices, err := loadDevices(paths.Devices, logger)
	if err != nil {
		errs = append(errs, err.Error())
	}

	// 2. Device groups ————————————————————————————————————————————————————————
	dgroups, err := loadDeviceGroups(paths.DeviceGroups, logger)
	if err != nil {
		errs = append(errs, err.Error())
	}

	// 3. Object groups ————————————————————————————————————————————————————————
	ogroups, err := loadObjectGroups(paths.ObjectGroups, logger)
	if err != nil {
		errs = append(errs, err.Error())
	}

	// 4. Object definitions (only those referenced by the resolved chain) ————
	needed := resolveNeededObjects(devices, dgroups, ogroups)
	logger.Info("config: resolved needed object keys from device→group→object chain", "needed", len(needed))

	objDefs, err := loadObjectDefs(paths.Objects, needed, logger)
	if err != nil {
		errs = append(errs, err.Error())
	}

	// 5. Enum definitions ————————————————————————————————————————————————————
	enumReg, err := loadEnums(paths.Enums, objDefs, logger)
	if err != nil {
		errs = append(errs, err.Error())
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("config: %d error(s):\n  %s", len(errs), strings.Join(errs, "\n  "))
	}

	return &LoadedConfig{
		Devices:      devices,
		DeviceGroups: dgroups,
		ObjectGroups: ogroups,
		ObjectDefs:   objDefs,
		Enums:        enumReg,
	}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Devices
// ─────────────────────────────────────────────────────────────────────────────

func loadDevices(dir string, logger *slog.Logger) (map[string]DeviceConfig, error) {
	result := make(map[string]DeviceConfig)
	files, err := yamlFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("list devices dir %q: %w", dir, err)
	}

	for _, path := range files {
		var raw map[string]rawDeviceEntry
		if err := decodeFile(path, &raw); err != nil {
			if errors.Is(err, errEmptyFile) {
				logger.Debug("config: skip empty device file", "file", path)
				continue
			}
			logger.Warn("config: skip malformed device file", "file", path, "error", err.Error())
			continue
		}
		for hostname, entry := range raw {
			result[hostname] = resolveDevice(entry)
		}
		logger.Debug("config: loaded device file", "file", path, "count", len(raw))
	}
	return result, nil
}

// resolveDevice applies hard-coded fallbacks for zero-valued fields,
// producing a fully-resolved DeviceConfig.
func resolveDevice(e rawDeviceEntry) DeviceConfig {
	port := e.Port
	if port == 0 {
		port = 161
	}

	interval := e.PollInterval
	if interval == 0 {
		interval = 60
	}

	timeout := e.Timeout
	if timeout == 0 {
		timeout = 3000
	}

	retries := e.Retries
	if retries == 0 {
		retries = 2
	}

	version := e.Version
	if version == "" {
		version = "2c"
	}

	maxPolls := e.MaxConcurrentPolls
	if maxPolls == 0 {
		maxPolls = 4
	}

	return DeviceConfig{
		IP:                 e.IP,
		Port:               port,
		PollInterval:       interval,
		Timeout:            timeout,
		Retries:            retries,
		ExponentialTimeout: e.ExponentialTimeout,
		Version:            version,
		Communities:        e.Communities,
		V3Credentials:      e.V3Credentials,
		DeviceGroups:       e.DeviceGroups,
		MaxConcurrentPolls: maxPolls,
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Device groups
// ─────────────────────────────────────────────────────────────────────────────

type rawDeviceGroupFile map[string]struct {
	ObjectGroups []string `yaml:"object_groups"`
}

func loadDeviceGroups(dir string, logger *slog.Logger) (map[string]DeviceGroup, error) {
	result := make(map[string]DeviceGroup)
	files, err := yamlFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("list device_groups dir %q: %w", dir, err)
	}

	for _, path := range files {
		var raw rawDeviceGroupFile
		if err := decodeFile(path, &raw); err != nil {
			if errors.Is(err, errEmptyFile) {
				logger.Debug("config: skip empty device_group file", "file", path)
				continue
			}
			logger.Warn("config: skip malformed device_group file", "file", path, "error", err.Error())
			continue
		}
		for name, g := range raw {
			result[name] = DeviceGroup{ObjectGroups: g.ObjectGroups}
		}
		logger.Debug("config: loaded device_groups file", "file", path, "count", len(raw))
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Object groups
// ─────────────────────────────────────────────────────────────────────────────

type rawObjectGroupFile map[string]struct {
	Objects []string `yaml:"objects"`
}

func loadObjectGroups(dir string, logger *slog.Logger) (map[string]ObjectGroup, error) {
	result := make(map[string]ObjectGroup)
	files, err := yamlFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("list object_groups dir %q: %w", dir, err)
	}

	for _, path := range files {
		var raw rawObjectGroupFile
		if err := decodeFile(path, &raw); err != nil {
			if errors.Is(err, errEmptyFile) {
				logger.Debug("config: skip empty object_group file", "file", path)
				continue
			}
			logger.Warn("config: skip malformed object_group file", "file", path, "error", err.Error())
			continue
		}
		for name, g := range raw {
			result[name] = ObjectGroup{Objects: g.Objects}
		}
		logger.Debug("config: loaded object_groups file", "file", path, "count", len(raw))
	}
	return result, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Object definitions
// ─────────────────────────────────────────────────────────────────────────────

// rawObjectFile is the top-level map: object_key → definition.
type rawObjectFile map[string]rawObjectBody

type rawObjectBody struct {
	MIB                string                      `yaml:"mib"`
	Object             string                      `yaml:"object"`
	Augments           string                      `yaml:"augments"`
	Index              []rawIndexBody              `yaml:"index"`
	DiscoveryAttribute string                      `yaml:"discovery_attribute"`
	Attributes         map[string]rawAttributeBody `yaml:"attributes"`
}

type rawIndexBody struct {
	Type   string `yaml:"type"`
	OID    string `yaml:"oid"`
	Name   string `yaml:"name"`
	Syntax string `yaml:"syntax"`
}

type rawAttributeBody struct {
	OID        string       `yaml:"oid"`
	Name       string       `yaml:"name"`
	Syntax     string       `yaml:"syntax"`
	Tag        bool         `yaml:"tag"`
	Overrides  *rawOverride `yaml:"overrides"`
	Rediscover string       `yaml:"rediscover"`
}

type rawOverride struct {
	Object    string `yaml:"object"`
	Attribute string `yaml:"attribute"`
}

// resolveNeededObjects walks devices → device_groups → object_groups and
// returns the set of object keys that are actually referenced. Only these
// keys need to be loaded from the objects directory, saving memory and
// startup time when the objects tree is large.
func resolveNeededObjects(
	devices map[string]DeviceConfig,
	dgroups map[string]DeviceGroup,
	ogroups map[string]ObjectGroup,
) map[string]bool {
	needed := make(map[string]bool)
	for _, dev := range devices {
		for _, dgName := range dev.DeviceGroups {
			dg, ok := dgroups[dgName]
			if !ok {
				continue
			}
			for _, ogName := range dg.ObjectGroups {
				og, ok := ogroups[ogName]
				if !ok {
					continue
				}
				for _, objKey := range og.Objects {
					needed[objKey] = true
				}
			}
		}
	}
	return needed
}

// loadObjectDefs loads object definition YAML files from dir.
// When needed is non-nil and non-empty, only object keys present in the set
// are kept — all others are discarded after parsing. When needed is nil the
// function loads everything (backwards-compatible behaviour).
func loadObjectDefs(dir string, needed map[string]bool, logger *slog.Logger) (map[string]models.ObjectDefinition, error) {
	result := make(map[string]models.ObjectDefinition)
	files, err := yamlFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, fmt.Errorf("list objects dir %q: %w", dir, err)
	}

	filterEnabled := len(needed) > 0
	var skipped int

	for _, path := range files {
		var raw rawObjectFile
		if err := decodeFile(path, &raw); err != nil {
			if errors.Is(err, errEmptyFile) {
				logger.Debug("config: skip empty object file", "file", path)
				continue
			}
			logger.Warn("config: skip malformed object file", "file", path, "error", err.Error())
			continue
		}
		for key, body := range raw {
			if filterEnabled && !needed[key] {
				skipped++
				continue
			}
			result[key] = convertObjectDef(key, body)
		}
		logger.Debug("config: loaded objects file", "file", path, "count", len(raw))
	}

	if filterEnabled {
		logger.Info("config: object filter applied",
			"kept", len(result),
			"skipped", skipped,
		)
	}
	return result, nil
}

func convertObjectDef(key string, b rawObjectBody) models.ObjectDefinition {
	index := make([]models.IndexDefinition, len(b.Index))
	for i, idx := range b.Index {
		index[i] = models.IndexDefinition{
			Type:   idx.Type,
			OID:    normaliseOID(idx.OID),
			Name:   idx.Name,
			Syntax: idx.Syntax,
		}
	}

	attrs := make(map[string]models.AttributeDefinition, len(b.Attributes))
	for name, a := range b.Attributes {
		var ovr *models.OverrideReference
		if a.Overrides != nil {
			ovr = &models.OverrideReference{
				Object:    a.Overrides.Object,
				Attribute: a.Overrides.Attribute,
			}
		}
		attrs[name] = models.AttributeDefinition{
			OID:        normaliseOID(a.OID),
			Name:       a.Name,
			Syntax:     a.Syntax,
			IsTag:      a.Tag,
			Overrides:  ovr,
			Rediscover: a.Rediscover,
		}
	}

	return models.ObjectDefinition{
		Key:                key,
		MIB:                b.MIB,
		Object:             b.Object,
		Augments:           b.Augments,
		Index:              index,
		DiscoveryAttribute: b.DiscoveryAttribute,
		Attributes:         attrs,
	}
}

// normaliseOID strips a leading dot so OIDs are in canonical form.
func normaliseOID(oid string) string {
	return strings.TrimPrefix(oid, ".")
}

// ─────────────────────────────────────────────────────────────────────────────
// Enum definitions
// ─────────────────────────────────────────────────────────────────────────────

// loadEnums reads every YAML file under dir and populates an EnumRegistry.
//
// It uses the loaded objectDefs to determine whether an OID's syntax is
// EnumBitmap so the bitmap flag is set correctly when registering.
func loadEnums(dir string, objectDefs map[string]models.ObjectDefinition, logger *slog.Logger) (*metrics.EnumRegistry, error) {
	reg := metrics.NewEnumRegistry()

	// Build a set of OIDs whose syntax is EnumBitmap (from object definitions).
	bitmapOIDs := make(map[string]bool)
	for _, def := range objectDefs {
		for _, attr := range def.Attributes {
			if attr.Syntax == "EnumBitmap" {
				bitmapOIDs[normaliseOID(attr.OID)] = true
			}
		}
	}

	files, err := yamlFiles(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return reg, nil
		}
		return reg, fmt.Errorf("list enums dir %q: %w", dir, err)
	}

	for _, path := range files {
		// Unmarshal as map[string]interface{} so we can type-switch the values.
		var raw map[string]interface{}
		if err := decodeFile(path, &raw); err != nil {
			if errors.Is(err, errEmptyFile) {
				logger.Debug("config: skip empty enum file", "file", path)
				continue
			}
			logger.Warn("config: skip malformed enum file", "file", path, "error", err.Error())
			continue
		}

		for oid, val := range raw {
			normOID := normaliseOID(oid)
			switch v := val.(type) {
			case string:
				// OID enum: value is a label string, key is the OID value.
				reg.RegisterOIDEnum(normOID, v)

			case map[string]interface{}:
				// Integer / bitmap enum: keys are integer values (as strings in yaml.v3).
				intMap, err := parseIntEnumMap(v)
				if err != nil {
					logger.Warn("config: skip unparseable int enum", "oid", oid, "error", err.Error())
					continue
				}
				reg.RegisterIntEnum(normOID, bitmapOIDs[normOID], intMap)

			case map[interface{}]interface{}:
				// yaml.v3 decodes YAML maps with integer keys as map[interface{}]interface{}.
				intMap, err := parseIntEnumMapGeneric(v)
				if err != nil {
					logger.Warn("config: skip unparseable int enum", "oid", oid, "error", err.Error())
					continue
				}
				reg.RegisterIntEnum(normOID, bitmapOIDs[normOID], intMap)

			default:
				logger.Warn("config: unknown enum value type", "oid", oid, "type", fmt.Sprintf("%T", val))
			}
		}
		logger.Debug("config: loaded enum file", "file", path)
	}
	return reg, nil
}

// parseIntEnumMap converts a map[string]interface{} (from YAML) into the
// map[int64]string that EnumRegistry expects.
func parseIntEnumMap(raw map[string]interface{}) (map[int64]string, error) {
	out := make(map[int64]string, len(raw))
	for k, v := range raw {
		i, err := strconv.ParseInt(fmt.Sprintf("%v", k), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("non-integer key %q: %w", k, err)
		}
		out[i] = fmt.Sprintf("%v", v)
	}
	return out, nil
}

// parseIntEnumMapGeneric converts a map[interface{}]interface{} (produced by
// yaml.v3 when YAML map keys are integers) into the map[int64]string that
// EnumRegistry expects.
func parseIntEnumMapGeneric(raw map[interface{}]interface{}) (map[int64]string, error) {
	out := make(map[int64]string, len(raw))
	for k, v := range raw {
		i, err := strconv.ParseInt(fmt.Sprintf("%v", k), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("non-integer key %v: %w", k, err)
		}
		out[i] = fmt.Sprintf("%v", v)
	}
	return out, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// yamlFiles returns all *.yml / *.yaml files under dir, sorted by path.
func yamlFiles(dir string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(p))
		if ext == ".yml" || ext == ".yaml" {
			paths = append(paths, p)
		}
		return nil
	})
	return paths, err
}

// errEmptyFile is returned by decodeFile when a YAML file contains no data
// (empty or comment-only). Callers should silently skip such files.
var errEmptyFile = errors.New("config: empty or comment-only YAML file")

// decodeFile opens path and unmarshals the YAML content into out.
// Returns errEmptyFile when the file is empty or contains only comments.
func decodeFile(path string, out interface{}) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dec := yaml.NewDecoder(f)
	dec.KnownFields(false) // be lenient — extra keys are fine
	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errEmptyFile
		}
		return err
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// no-op logger writer
// ─────────────────────────────────────────────────────────────────────────────

type noopWriter struct{}

func (noopWriter) Write(p []byte) (int, error) { return len(p), nil }
