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

	decrypted, err := Decrypt("v1:Ky78R73VoVfLZJGO:SwA6D9gcd8RN64zOjpyJsx2K5g8=")
	require.NoError(t, err)
	require.Equal(t, "test", decrypted)
}

func TestEncryptDecrypt(t *testing.T) {
	os.Setenv("PULUMI_CONFIG_PASSPHRASE", "foo")
	salt := "v1:LAQ7P6sT/+w=:v1:WejwuMb5G4TZsR/r:xZvrv45hbT2QRrHCkQrepVv3xQfMjw=="
	err := initCrypter(salt)
	require.NoError(t, err)
	encrypted, err := Encrypt("test")
	require.NoError(t, err)
	require.NotEmpty(t, encrypted)

	decrypted, err := Decrypt(encrypted)
	require.NoError(t, err)
	require.Equal(t, "test", decrypted)
}
