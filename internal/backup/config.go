package backup

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed config.schema.json
var configSchema []byte

//go:embed manifest.schema.json
var manifestSchema []byte

type PostgreSQL struct {
	MajorVersion           int `json:"majorVersion"`
	TargetServerMajor      int `json:"targetServerMajor"`
	ConnectTimeoutSeconds  int `json:"connectTimeoutSeconds"`
	LockWaitTimeoutSeconds int `json:"lockWaitTimeoutSeconds"`
}
type Filters struct {
	IncludeSchemas      []string `json:"includeSchemas"`
	ExcludeSchemas      []string `json:"excludeSchemas"`
	ExcludeTables       []string `json:"excludeTables"`
	ExcludeTableData    []string `json:"excludeTableData"`
	IncludeLargeObjects bool     `json:"includeLargeObjects"`
	IncludeACL          bool     `json:"includeAcl"`
	StrictNames         bool     `json:"strictNames"`
	CompressionLevel    int      `json:"compressionLevel"`
}
type RequiredTable struct {
	Schema      string `json:"schema"`
	Table       string `json:"table"`
	RequireData bool   `json:"requireData"`
}
type Validation struct {
	RequiredSchemas     []string        `json:"requiredSchemas"`
	RequiredTables      []RequiredTable `json:"requiredTables"`
	MinTableDataEntries int             `json:"minTableDataEntries"`
}
type S3Config struct {
	Endpoint                   string `json:"endpoint"`
	Region                     string `json:"region"`
	Bucket                     string `json:"bucket"`
	Prefix                     string `json:"prefix"`
	AddressingStyle            string `json:"addressingStyle"`
	SignatureVersion           string `json:"signatureVersion"`
	ConnectTimeoutSeconds      int    `json:"connectTimeoutSeconds"`
	ReadTimeoutSeconds         int    `json:"readTimeoutSeconds"`
	TotalAttempts              int    `json:"totalAttempts"`
	MultipartThresholdMiB      int64  `json:"multipartThresholdMiB"`
	MultipartChunkSizeMiB      int64  `json:"multipartChunkSizeMiB"`
	MaxConcurrency             int    `json:"maxConcurrency"`
	ReadChunkMiB               int    `json:"readChunkMiB"`
	RequestChecksumCalculation string `json:"requestChecksumCalculation"`
	ResponseChecksumValidation string `json:"responseChecksumValidation"`
}
type Legacy struct {
	Enabled           bool   `json:"enabled"`
	DryRun            bool   `json:"dryRun"`
	Prefix            string `json:"prefix"`
	FilenamePattern   string `json:"filenamePattern"`
	MinVerifiedV2Sets int    `json:"minVerifiedV2Sets"`
}
type Retention struct {
	Enabled                 bool   `json:"enabled"`
	DryRun                  bool   `json:"dryRun"`
	DeletionApproved        bool   `json:"deletionApproved"`
	MaxAgeDays              int    `json:"maxAgeDays"`
	KeepLast                int    `json:"keepLast"`
	IncompleteRunMaxAgeDays int    `json:"incompleteRunMaxAgeDays"`
	Legacy                  Legacy `json:"legacy"`
}
type SizePolicy struct {
	HistorySamples int     `json:"historySamples"`
	MinSamples     int     `json:"minSamples"`
	MinRatio       float64 `json:"minRatio"`
	MaxRatio       float64 `json:"maxRatio"`
}
type Config struct {
	ConfigVersion int        `json:"configVersion"`
	PostgreSQL    PostgreSQL `json:"postgresql"`
	Backup        Filters    `json:"backup"`
	Validation    Validation `json:"validation"`
	S3            S3Config   `json:"s3"`
	Process       struct {
		ShutdownGraceSeconds int `json:"shutdownGraceSeconds"`
	} `json:"process"`
	Retention     Retention `json:"retention"`
	Observability struct {
		SizeAnomaly SizePolicy `json:"sizeAnomaly"`
	} `json:"observability"`
}

// Validate JSON before decoding into structs: missing false/zero fields and duplicate
// keys must not silently become defaults. Errors never include input values.
func decode(data, schema []byte, target any) error {
	if err := uniqueJSON(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return errors.New("INVALID_JSON")
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return errors.New("INVALID_JSON")
	}
	resource, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return errors.New("INVALID_SCHEMA")
	}
	compiler := jsonschema.NewCompiler()
	if err = compiler.AddResource("schema.json", resource); err != nil {
		return errors.New("INVALID_SCHEMA")
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return errors.New("INVALID_SCHEMA")
	}
	if compiled.Validate(doc) != nil {
		return errors.New("INVALID_CONFIG")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(target) != nil {
		return errors.New("INVALID_JSON")
	}
	return nil
}
func uniqueJSON(d *json.Decoder) error {
	var visit func() error
	visit = func() error {
		tok, err := d.Token()
		if err != nil {
			return err
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{':
				keys := map[string]bool{}
				for d.More() {
					key, err := d.Token()
					if err != nil {
						return err
					}
					name, ok := key.(string)
					if !ok || keys[name] {
						return errors.New("DUPLICATE_KEY")
					}
					keys[name] = true
					if err = visit(); err != nil {
						return err
					}
				}
			case '[':
				for d.More() {
					if err = visit(); err != nil {
						return err
					}
				}
			default:
				return errors.New("INVALID_JSON")
			}
			_, err = d.Token()
			return err
		}
		return nil
	}
	if err := visit(); err != nil {
		return err
	}
	_, err := d.Token()
	if err != io.EOF {
		return errors.New("TRAILING_JSON")
	}
	return nil
}
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, errors.New("CONFIG_UNREADABLE")
	}
	return ParseConfig(data)
}
func ParseConfig(data []byte) (Config, error) {
	var c Config
	if err := decode(data, configSchema, &c); err != nil {
		return c, err
	}
	u, err := url.Parse(c.S3.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return c, errors.New("INVALID_ENDPOINT")
	}
	for _, part := range strings.Split(strings.TrimSuffix(c.S3.Prefix, "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return c, errors.New("INVALID_PREFIX")
		}
	}
	if c.PostgreSQL.MajorVersion != c.PostgreSQL.TargetServerMajor {
		return c, errors.New("TARGET_MAJOR_MISMATCH")
	}
	l := c.Retention.Legacy
	if l.Enabled {
		if l.Prefix == "" || !strings.HasPrefix(l.FilenamePattern, "^") || !strings.HasSuffix(l.FilenamePattern, "$") {
			return c, errors.New("INVALID_LEGACY_SCOPE")
		}
		if _, err = regexp.Compile(l.FilenamePattern); err != nil {
			return c, errors.New("INVALID_LEGACY_PATTERN")
		}
	}
	if c.Retention.MaxAgeDays > 36500 || c.Retention.IncompleteRunMaxAgeDays > 36500 {
		return c, errors.New("INVALID_RETENTION_DURATION")
	}
	if !c.Retention.DryRun && !c.Retention.DeletionApproved {
		return c, errors.New("DELETION_NOT_APPROVED")
	}
	p := c.Observability.SizeAnomaly
	if p.MinSamples > p.HistorySamples || p.MinRatio >= p.MaxRatio {
		return c, errors.New("INVALID_SIZE_POLICY")
	}
	return c, nil
}

type Credentials struct{ AccessKey, SecretKey, Token string }

func LoadCredentials(dir string) (Credentials, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b)), err
	}
	a, err := read("access_key")
	if err != nil || a == "" {
		return Credentials{}, errors.New("MISSING_CREDENTIALS")
	}
	s, err := read("secret_key")
	if err != nil || s == "" {
		return Credentials{}, errors.New("MISSING_CREDENTIALS")
	}
	t, err := read("session_token")
	if err != nil && !os.IsNotExist(err) {
		return Credentials{}, errors.New("TOKEN_UNREADABLE")
	}
	return Credentials{a, s, t}, nil
}
