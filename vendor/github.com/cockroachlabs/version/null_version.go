package version

import (
	"database/sql/driver"
	"encoding/json"

	"github.com/cockroachdb/errors"
)

// Represents a NULLable version when stored in the database. The zero
// value of NullVersion serializes as database NULL (and vice-versa).
type NullVersion struct {
	Valid   bool
	Version Version
}

func NewNullVersion(v Version) NullVersion {
	return NullVersion{
		Valid:   !v.Empty(),
		Version: v,
	}
}

// Value is used when serializing a NullVersion for storage in the db.
func (n NullVersion) Value() (driver.Value, error) {
	if n.Valid {
		return n.Version.String(), nil
	} else {
		return nil, nil
	}
}

// Scan implements sql.Scanner, and is used when deserializing a NullVersion from the db.
func (n *NullVersion) Scan(value interface{}) error {
	if value == nil {
		*n = NullVersion{Valid: false, Version: Version{}}
		return nil
	}
	err := n.Version.Scan(value)
	if err != nil {
		return err
	}
	n.Valid = true
	return nil
}

// We must implement json.Unmarshaler, because the invalid NullVersion stores an empty
// string in the version field, and we don't want to make Version unmarshal successfully
// from empty string (it should and does maintain the same behavior as Parse).
func (n *NullVersion) UnmarshalJSON(data []byte) error {
	var rawMap map[string]interface{}
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return err
	}
	if valid, ok := rawMap["Valid"].(bool); ok && !valid {
		n.Valid = false
		n.Version = Version{}
		return nil
	} else if ok && valid {
		// then Version is a map like {"$raw": "vX.Y.Z"}
		if versionMap, ok := rawMap["Version"].(map[string]interface{}); ok {
			if rawVersion, ok := versionMap["$raw"].(string); ok {
				parsed, err := Parse(rawVersion)
				if err != nil {
					return err
				}
				n.Valid = true
				n.Version = parsed
				return nil
			}
		}
	}
	return errors.Newf("cannot parse '%s' as NullVersion", data)
}
