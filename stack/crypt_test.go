package stack

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEncrypt(t *testing.T) {
	os.Setenv("PULUMI_CONFIG_PASSPHRASE", "foo")
	salt := "v1:LAQ7P6sT/+w=:v1:WejwuMb5G4TZsR/r:xZvrv45hbT2QRrHCkQrepVv3xQfMjw=="
	err := initCrypter(salt)
	require.NoError(t, err)
	encrypted, err := Encrypt("test")
	require.NoError(t, err)
	require.NotEmpty(t, encrypted)
}

func TestDecrypt(t *testing.T) {
	os.Setenv("PULUMI_CONFIG_PASSPHRASE", "foo")
	salt := "v1:LAQ7P6sT/+w=:v1:WejwuMb5G4TZsR/r:xZvrv45hbT2QRrHCkQrepVv3xQfMjw=="
	err := initCrypter(salt)
	require.NoError(t, err)
	decrypted, err := Decrypt("v1:6rgJQkU0YY5NFozT:HN1XFZSfr2YW1Pi1QtMfUJjad4w=")
	require.NoError(t, err)
	require.Equal(t, "test", decrypted)
}
