package stack

import (
	"context"
	"errors"
	"os"

	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
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

var secretsManager secrets.Manager

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
	if secretsManager != nil {
		return nil
	}

	pp := os.Getenv("PULUMI_CONFIG_PASSPHRASE")
	if pp == "" {
		return errors.New("PULUMI_CONFIG_PASSPHRASE is not set")
	}
	var err error
	secretsManager, err = passphrase.GetPassphraseSecretsManager(pp, salt)
	if err != nil {
		return err
	}

	return nil
}

func Encrypt(value string) (string, error) {
	if secretsManager == nil {
		return "", errors.New("secretsManager is not initialized")
	}

	enc := secretsManager.Encrypter()
	encrypted, err := enc.EncryptValue(context.Background(), value)
	if err != nil {
		return "", err
	}
	return encrypted, nil
}

func Decrypt(value string) (string, error) {
	if secretsManager == nil {
		return "", errors.New("secretsManager is not initialized")
	}

	dec := secretsManager.Decrypter()
	decrypted, err := dec.DecryptValue(context.Background(), value)
	if err != nil {
		return "", err
	}
	return decrypted, nil
}
