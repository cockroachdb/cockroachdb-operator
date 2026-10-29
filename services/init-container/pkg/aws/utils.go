package aws

import (
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/sts"
	"github.com/cockroachdb/errors"
	"github.com/gofrs/uuid"
)

func AssumeRole(
	httpClient *http.Client,
	accessKeyID string,
	secretAccessKey string,
	externalID string,
	authPrincipal string,
) (*sts.Credentials, error) {
	// Start a session with our own user credentials. These
	// credentials are created when the cluster is created, and are meant
	// to be used exclusively for the purpose of CMEK.
	userSession, err := session.NewSession(&aws.Config{
		Credentials: credentials.NewStaticCredentials(accessKeyID, secretAccessKey, ""),
		HTTPClient:  httpClient,
	})
	if err != nil {
		return nil, errors.Wrap(err, "error starting user session")
	}

	// Generate the structures necessary for our user to assume the
	// customer's KMS role.
	roleSessionUUID, err := uuid.NewV4()
	if err != nil {
		return nil, errors.Wrap(err, "error generating role session UUID")
	}
	roleSessionName := fmt.Sprintf("crl-%s", roleSessionUUID.String())
	assumeRoleInput := &sts.AssumeRoleInput{
		ExternalId:      &externalID,
		RoleArn:         &authPrincipal,
		RoleSessionName: &roleSessionName,
	}
	stsClient := sts.New(userSession)

	// Using our own user credentials, assume the KMS role that has been
	// provided to us by the customer.
	assumeRoleOutput, err := stsClient.AssumeRole(assumeRoleInput)
	if err != nil {
		return nil, errors.Wrap(err, "error assuming KMS role")
	}

	// Verify that we have the proper role credentials.
	if assumeRoleOutput == nil || assumeRoleOutput.Credentials == nil {
		return nil, errors.New("output of KMS AssumeRole call is missing information")
	}
	roleCredentials := assumeRoleOutput.Credentials
	if roleCredentials.AccessKeyId == nil || roleCredentials.SecretAccessKey == nil || roleCredentials.SessionToken == nil {
		return nil, errors.New("output of KMS AssumeRole call is missing information")
	}

	return roleCredentials, nil
}
