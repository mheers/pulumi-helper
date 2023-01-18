package stack

import (
	"context"
	"errors"
	"os"

	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/sirupsen/logrus"
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
	pp := os.Getenv("PULUMI_CONFIG_PASSPHRASE")
	if pp == "" {
		return errors.New("PULUMI_CONFIG_PASSPHRASE is not set")
	}
	sm, err := passphrase.NewPassphraseSecretsManager(pp, salt)
	if err != nil {
		return err
	}
	secretsManager = sm
	return nil
}

func Encrypt(value string) string {
	enc, err := secretsManager.Encrypter()
	if err != nil {
		logrus.Fatal(err)
	}
	encrypted, err := enc.EncryptValue(context.Background(), value)
	if err != nil {
		logrus.Fatal(err)
	}
	return encrypted
}

func Decrypt(value string) string {
	dec, err := secretsManager.Decrypter()
	if err != nil {
		logrus.Fatal(err)
	}
	decrypted, err := dec.DecryptValue(context.Background(), value)
	if err != nil {
		logrus.Fatal(err)
	}
	return decrypted
}
