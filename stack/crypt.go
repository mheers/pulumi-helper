package stack

import (
	"context"
	"errors"
	"os"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

func encryptionSalt(ctx *pulumi.Context) (string, error) {
	stackName := ctx.Stack()
	return encryptionSaltByStackName(stackName)
}

func encryptionSaltByStackName(stackName string) (string, error) {
	y, err := ReadStackYaml(stackName)
	if err != nil {
		return "", err
	}
	return y.Encryptionsalt, nil
}

var crypter config.Crypter

func InitCrypterWithSaltAndPassphrase(salt, passphrase string) error {
	if err := os.Setenv("PULUMI_CONFIG_PASSPHRASE", passphrase); err != nil {
		return err
	}
	return initCrypter(salt)
}

func InitCrypter(ctx *pulumi.Context) error {
	salt, err := encryptionSalt(ctx)
	if err != nil {
		return err
	}
	return initCrypter(salt)
}

func InitCrypterForProject(name string) error {
	salt, err := encryptionSaltByStackName(name)
	if err != nil {
		return err
	}
	return initCrypter(salt)
}

func initCrypter(salt string) error {
	// only initialize once
	if crypter != nil {
		return nil
	}

	pp := os.Getenv("PULUMI_CONFIG_PASSPHRASE")
	if pp == "" {
		return errors.New("PULUMI_CONFIG_PASSPHRASE is not set")
	}
	crypter = config.NewSymmetricCrypterFromPassphrase(pp, []byte(salt))

	return nil
}

func Encrypt(value string) (string, error) {
	if crypter == nil {
		return "", errors.New("secretsManager is not initialized")
	}

	encrypted, err := crypter.EncryptValue(context.Background(), value)
	if err != nil {
		return "", err
	}
	return encrypted, nil
}

func Decrypt(value string) (string, error) {
	if crypter == nil {
		return "", errors.New("secretsManager is not initialized")
	}

	decrypted, err := crypter.DecryptValue(context.Background(), value)
	if err != nil {
		return "", err
	}
	return decrypted, nil
}
