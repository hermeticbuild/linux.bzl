package ccprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func rejectDuplicateKeys(data []byte) error {
	return rejectDuplicateKeysNamed(data, "JSON document")
}

func rejectDuplicateKeysNamed(data []byte, context string) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := consumeJSONValue(decoder, "$"); err != nil {
		return fmt.Errorf("decode %s: %w", context, err)
	}
	return requireJSONEOFNamed(decoder, context)
}

func consumeJSONValue(decoder *json.Decoder, path string) error {
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
		seen := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s contains a non-string object key", path)
			}
			if seen[key] {
				return fmt.Errorf("%s contains duplicate key %q", path, key)
			}
			seen[key] = true
			if err := consumeJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("%s object has invalid terminator", path)
		}
	case '[':
		index := 0
		for decoder.More() {
			if err := consumeJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("%s array has invalid terminator", path)
		}
	default:
		return fmt.Errorf("%s has unexpected delimiter %q", path, delimiter)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	return requireJSONEOFNamed(decoder, "JSON document")
}

func requireJSONEOFNamed(decoder *json.Decoder, context string) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode %s: trailing JSON value", context)
		}
		return fmt.Errorf("decode %s: %w", context, err)
	}
	return nil
}
