package denylist

import (
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"gopkg.in/yaml.v2"
)

func Deserialize(input []byte) (*File, error) {
	var file File
	if err := yaml.Unmarshal(input, &file); err != nil {
		return nil, errors.Wrap(err, "unable to unmarshal the denylist")
	}
	return &file, nil
}

func (file *File) Serialize() ([]byte, error) {
	return yaml.Marshal(file)
}

// File is the yaml format for the sqlproxy's denylist config.
type File struct {
	Seq      int64        `yaml:"SequenceNumber"`
	Denylist []*DenyEntry `yaml:"denylist"`
}

// DenyEntry records info about one denied entity,
// the reason and the expiration time.
// This also serves as spec for the yaml config format.
type DenyEntry struct {
	Entity     DenyEntity `yaml:"entity"`
	Expiration time.Time  `yaml:"expiration"`
	Reason     string     `yaml:"reason"`
}

// DenyEntity represent one denied entity.
// This also serves as the spec for the config format.
type DenyEntity struct {
	Item string `yaml:"item"`
	Type Type   `yaml:"type"`
}

// Type is the type of the denied entity.
type Type int

// Enum values for Type.
const (
	IPAddrType Type = iota + 1
	ClusterType
	UnknownType
)

var strToTypeMap = map[string]Type{
	"ip":      IPAddrType,
	"cluster": ClusterType,
}

var typeToStrMap = map[Type]string{
	IPAddrType:  "ip",
	ClusterType: "cluster",
}

// UnmarshalYAML implements yaml.Unmarshaler interface for type.
func (typ *Type) UnmarshalYAML(unmarshal func(any) error) error {
	var raw string
	err := unmarshal(&raw)
	if err != nil {
		return err
	}

	normalized := strings.ToLower(raw)
	t, ok := strToTypeMap[normalized]
	if !ok {
		*typ = UnknownType
	} else {
		*typ = t
	}

	return nil
}

// MarshalYAML implements yaml.Marshaler interface for type.
func (typ Type) MarshalYAML() (any, error) {
	return typ.String(), nil
}

// String implements Stringer interface for type.
func (typ Type) String() string {
	s, ok := typeToStrMap[typ]
	if !ok {
		return "UNKNOWN"
	}
	return s
}
