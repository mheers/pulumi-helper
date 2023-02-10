package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestJSONBytesToYamlBytes(t *testing.T) {
	jsonBytes := []byte(`{"name": "marcel", "hobbies": ["music", "computer", "sport"]}`)

	yamlBytes := []byte(`hobbies:
- music
- computer
- sport
name: marcel
`)

	y, err := JsonBytesToYamlBytes(jsonBytes)
	require.Nil(t, err)
	require.NotEmpty(t, y)
	require.Equal(t, yamlBytes, y)
}
