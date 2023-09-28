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

	decrypted, err := Decrypt("v1:fYYADOWNT7IqCV0V:DrMqOwJhAMQPuc6GssWyi7ggM9Y=")
	require.NoError(t, err)
	require.Equal(t, "test", decrypted)
}

func TestDecryptOld(t *testing.T) {
	os.Setenv("PULUMI_CONFIG_PASSPHRASE", "foo")
	salt := "v1:kHqNYXEuUOY=:v1:iV9u6JT0OpQFvpqQ:yXm3mSW5jO5t+uLigEmEbpXsflDxMQ=="
	err := initCrypter(salt)
	require.NoError(t, err)

	decrypted, err := Decrypt("v1:D8D7cmOhI3pMhAG5:UOW+JAdt1vX/GrJSQoiwWXMGEgacCCGCuzgbe2vvYw==")
	require.NoError(t, err)
	require.Equal(t, "jr7$sZS!vfPuJlM", decrypted)
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
