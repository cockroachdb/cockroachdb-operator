package safesql

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/lib/pq"
)

// SQLStringer is an interface that can be implemented by types to customize how
// they are formatted into SQL by Format.
type SQLStringer interface {
	SQLString() string
}

// TODO(peter): @knz says: the quoting rules for usernames are different from
// both strings and identifiers! So you might want to do something similar to
// crdb and make security.SQLUsername a special case here.

// Identifier provides proper quoting of SQL identifiers (such as column,
// table, and database names) when used as arguments to Format.
type Identifier string

// SQLString implements the SQLStringer interface.
func (i Identifier) SQLString() string {
	return pq.QuoteIdentifier(string(i))
}

// QualifiedIdentifier provides proper quoting of a qualified identifier
// with an arbitrary number of components (e.g. <schema>.<table>.<column>)
// when used as an argument to Format.
type QualifiedIdentifier []string

// SQLString implements the SQLStringer interface.
func (qi QualifiedIdentifier) SQLString() string {
	quotedParts := make([]string, len(qi))
	for i, part := range qi {
		quotedParts[i] = Identifier(part).SQLString()
	}
	return strings.Join(quotedParts, ".")
}

// SQL encapsulates a known safe SQL fragment. It should not be used with SQL
// from an external source or SQL containing identifiers.
//
// Use of this type presents a security risk: the encapsulated content should
// come from a trusted source, as it will be included verbatim in the output
// when used as an argument to Format().
type SQL string

// SQLString implements the SQLStringer interface.
func (s SQL) SQLString() string {
	return string(s)
}

func parseArgIdx(s string, i int) (idx, newI int) {
	if i >= len(s) || s[i] < '0' || s[i] > '9' {
		// Not a valid number.
		return -1, i
	}

	end := len(s)
	for ; i < end && '0' < s[i] && s[i] <= '9'; i++ {
		if idx >= 1e6 {
			// Too large an argument index specifier.
			return -1, i
		}
		idx = idx*10 + int(s[i]-'0')
	}
	return idx, i
}

// Format formats a SQL string, recognizing %1, %2, etc as placeholders and
// replacing them with the corresponding arguments (1-indexed). Unlike
// printf-style formatting, the placeholders must be preceded by punctuation or
// whitespace. This prevents errors such as `xxx_%1` from being formatted as
// `xxx_'%1'` which is almost certainly incorrect.
//
// Formatting of the argument proceeds by the following rules:
//
// - if args[i] has a SQLString() method, it is used
// - if args[i] is a string type, it is quoted as a literal (pq.QuoteLiteral)
// - if args[i] is an integer type, it is formatted as %d
// - if args[i] is an integer type, it is formatted as %d
// - if args[i] is a float type, it is formatted as %f
// - otherwise, panic with an error
func Format(format string, args ...any) string {
	var b strings.Builder

	// The loop structure here is adapted from the stdlib fmt.Sprintf code, but
	// heavily simplified because we don't have to support different verbs, only
	// formatting of different types.
	for i, end := 0, len(format); i < end; {
		start := i
		for ; i < end && format[i] != '%'; i++ {
		}
		if start < i {
			b.WriteString(format[start:i])
		}
		if i >= end {
			break
		}

		if i > 0 {
			r, _ := utf8.DecodeLastRuneInString(format[:i])
			switch {
			case r == '=':
			case unicode.IsPunct(r):
			case unicode.IsSpace(r):
			default:
				panic(fmt.Errorf("invalid separator: %q is not punctuation or whitespace", r))
			}
		}

		start = i
		i++
		if i < end && format[i] == '%' {
			i++
			b.WriteString("%")
			continue
		}

		var argIdx int
		argIdx, i = parseArgIdx(format, i)
		if argIdx == -1 {
			panic(fmt.Errorf("invalid placeholder: %s", format[start:]))
		}
		if argIdx < 1 || argIdx > len(args) {
			panic(fmt.Errorf("bad placeholder index: %s", format[start:]))
		}

		// Note that we explicitly do not use fmt.Fprintf below because we want to
		// carefully control how the type is formatted and not accidentally use an
		// arg.Format() method.
		arg := args[argIdx-1]
		err := writeFormattedArg(arg, &b)
		if err != nil {
			panic(fmt.Errorf("bad argument %d: %w", argIdx, err))
		}
	}

	return b.String()
}

// Join concatenates the given elements with the given separator, formatting each element per Format.
// A type parameter is used here as we typically expect to see a slice of a concrete type passed as `args`
// and using one avoids the need to convert the slice.
func Join[T any](args []T, sep SQL) SQL {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteString(sep.SQLString())
		}
		err := writeFormattedArg(arg, &b)
		if err != nil {
			panic(fmt.Errorf("bad argument %d: %w", i, err))
		}
	}

	return SQL(b.String())
}

func writeFormattedArg(arg any, b *strings.Builder) error {
	switch t := arg.(type) {
	case SQLStringer:
		b.WriteString(t.SQLString())

	default:
		v := reflect.ValueOf(arg)
		switch v.Kind() {
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			b.WriteString(strconv.FormatInt(v.Int(), 10))

		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			b.WriteString(strconv.FormatUint(v.Uint(), 10))

		case reflect.Float32:
			b.WriteString(strconv.FormatFloat(v.Float(), 'f', -1, 32))

		case reflect.Float64:
			b.WriteString(strconv.FormatFloat(v.Float(), 'f', -1, 64))

		case reflect.String:
			b.WriteString(pq.QuoteLiteral(v.String()))

		default:
			return fmt.Errorf("unsupported type: %T", arg)
		}
	}

	return nil
}
