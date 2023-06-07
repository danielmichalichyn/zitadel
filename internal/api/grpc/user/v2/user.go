package user

import (
	"context"
	"encoding/base64"
	"io"

	"golang.org/x/text/language"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/zitadel/zitadel/internal/api/authz"
	"github.com/zitadel/zitadel/internal/api/grpc/object/v2"
	"github.com/zitadel/zitadel/internal/command"
	"github.com/zitadel/zitadel/internal/crypto"
	"github.com/zitadel/zitadel/internal/domain"
	"github.com/zitadel/zitadel/internal/errors"
	object_pb "github.com/zitadel/zitadel/pkg/grpc/object/v2alpha"
	user "github.com/zitadel/zitadel/pkg/grpc/user/v2alpha"
)

func (s *Server) AddHumanUser(ctx context.Context, req *user.AddHumanUserRequest) (_ *user.AddHumanUserResponse, err error) {
	human, err := addUserRequestToAddHuman(req)
	if err != nil {
		return nil, err
	}
	orgID := authz.GetCtxData(ctx).OrgID
	err = s.command.AddHuman(ctx, orgID, human, false)
	if err != nil {
		return nil, err
	}
	return &user.AddHumanUserResponse{
		UserId:    human.ID,
		Details:   object.DomainToDetailsPb(human.Details),
		EmailCode: human.EmailCode,
	}, nil
}

func addUserRequestToAddHuman(req *user.AddHumanUserRequest) (*command.AddHuman, error) {
	username := req.GetUsername()
	if username == "" {
		username = req.GetEmail().GetEmail()
	}
	var urlTemplate string
	if req.GetEmail().GetSendCode() != nil {
		urlTemplate = req.GetEmail().GetSendCode().GetUrlTemplate()
		// test the template execution so the async notification will not fail because of it and the user won't realize
		if err := domain.RenderConfirmURLTemplate(io.Discard, urlTemplate, req.GetUserId(), "code", "orgID"); err != nil {
			return nil, err
		}
	}
	bcryptedPassword, err := hashedPasswordToCommand(req.GetHashedPassword())
	if err != nil {
		return nil, err
	}
	passwordChangeRequired := req.GetPassword().GetChangeRequired() || req.GetHashedPassword().GetChangeRequired()
	metadata := make([]*command.AddMetadataEntry, len(req.Metadata))
	for i, metadataEntry := range req.Metadata {
		metadata[i] = &command.AddMetadataEntry{
			Key:   metadataEntry.GetKey(),
			Value: metadataEntry.GetValue(),
		}
	}
	links := make([]*command.AddLink, len(req.GetIdpLinks()))
	for i, link := range req.GetIdpLinks() {
		links[i] = &command.AddLink{
			IDPID:         link.GetIdpId(),
			IDPExternalID: link.GetIdpExternalId(),
			DisplayName:   link.GetDisplayName(),
		}
	}
	return &command.AddHuman{
		ID:          req.GetUserId(),
		Username:    username,
		FirstName:   req.GetProfile().GetFirstName(),
		LastName:    req.GetProfile().GetLastName(),
		NickName:    req.GetProfile().GetNickName(),
		DisplayName: req.GetProfile().GetDisplayName(),
		Email: command.Email{
			Address:     domain.EmailAddress(req.GetEmail().GetEmail()),
			Verified:    req.GetEmail().GetIsVerified(),
			ReturnCode:  req.GetEmail().GetReturnCode() != nil,
			URLTemplate: urlTemplate,
		},
		PreferredLanguage:      language.Make(req.GetProfile().GetPreferredLanguage()),
		Gender:                 genderToDomain(req.GetProfile().GetGender()),
		Phone:                  command.Phone{}, // TODO: add as soon as possible
		Password:               req.GetPassword().GetPassword(),
		BcryptedPassword:       bcryptedPassword,
		PasswordChangeRequired: passwordChangeRequired,
		Passwordless:           false,
		Register:               false,
		Metadata:               metadata,
		Links:                  links,
	}, nil
}

func genderToDomain(gender user.Gender) domain.Gender {
	switch gender {
	case user.Gender_GENDER_UNSPECIFIED:
		return domain.GenderUnspecified
	case user.Gender_GENDER_FEMALE:
		return domain.GenderFemale
	case user.Gender_GENDER_MALE:
		return domain.GenderMale
	case user.Gender_GENDER_DIVERSE:
		return domain.GenderDiverse
	default:
		return domain.GenderUnspecified
	}
}

func hashedPasswordToCommand(hashed *user.HashedPassword) (string, error) {
	if hashed == nil {
		return "", nil
	}
	// we currently only handle bcrypt
	if hashed.GetAlgorithm() != "bcrypt" {
		return "", errors.ThrowInvalidArgument(nil, "USER-JDk4t", "Errors.InvalidArgument")
	}
	return hashed.GetHash(), nil
}

func (s *Server) AddIDPLink(ctx context.Context, req *user.AddIDPLinkRequest) (_ *user.AddIDPLinkResponse, err error) {
	orgID := authz.GetCtxData(ctx).OrgID
	details, err := s.command.AddUserIDPLink(ctx, req.UserId, orgID, &domain.UserIDPLink{
		IDPConfigID:    req.GetIdpLink().GetIdpId(),
		ExternalUserID: req.GetIdpLink().GetIdpExternalId(),
		DisplayName:    req.GetIdpLink().GetDisplayName(),
	})
	if err != nil {
		return nil, err
	}
	return &user.AddIDPLinkResponse{
		Details: object.DomainToDetailsPb(details),
	}, nil
}

func (s *Server) StartIdentityProviderFlow(ctx context.Context, req *user.StartIdentityProviderFlowRequest) (_ *user.StartIdentityProviderFlowResponse, err error) {
	id, details, err := s.command.CreateIntent(ctx, req.GetIdpId(), req.GetSuccessUrl(), req.GetFailureUrl(), authz.GetCtxData(ctx).OrgID)
	if err != nil {
		return nil, err
	}
	authURL, err := s.command.AuthURLFromProvider(ctx, req.GetIdpId(), id, s.idpCallback(ctx))
	if err != nil {
		return nil, err
	}
	return &user.StartIdentityProviderFlowResponse{
		Details:  object.DomainToDetailsPb(details),
		NextStep: &user.StartIdentityProviderFlowResponse_AuthUrl{AuthUrl: authURL},
	}, nil
}

func (s *Server) RetrieveIdentityProviderInformation(ctx context.Context, req *user.RetrieveIdentityProviderInformationRequest) (_ *user.RetrieveIdentityProviderInformationResponse, err error) {
	intent, err := s.command.GetIntentWriteModel(ctx, req.GetIntentId(), authz.GetCtxData(ctx).OrgID)
	if err != nil {
		return nil, err
	}
	if err := s.checkIntentToken(req.GetToken(), intent.AggregateID); err != nil {
		return nil, err
	}
	if intent.State != domain.IDPIntentStateSucceeded {
		return nil, errors.ThrowPreconditionFailed(nil, "IDP-Hk38e", "Errors.Intent.NotSucceeded")
	}
	return intentToIDPInformationPb(intent, s.idpAlg)
}

func intentToIDPInformationPb(intent *command.IDPIntentWriteModel, alg crypto.EncryptionAlgorithm) (_ *user.RetrieveIdentityProviderInformationResponse, err error) {
	var idToken *string
	if intent.IDPIDToken != "" {
		idToken = &intent.IDPIDToken
	}
	var accessToken string
	if intent.IDPAccessToken != nil {
		accessToken, err = crypto.DecryptString(intent.IDPAccessToken, alg)
		if err != nil {
			return nil, err
		}
	}
	return &user.RetrieveIdentityProviderInformationResponse{
		Details: &object_pb.Details{
			Sequence:      intent.ProcessedSequence,
			ChangeDate:    timestamppb.New(intent.ChangeDate),
			ResourceOwner: intent.ResourceOwner,
		},
		IdpInformation: &user.IDPInformation{
			Access: &user.IDPInformation_Oauth{
				Oauth: &user.IDPOAuthAccessInformation{
					AccessToken: accessToken,
					IdToken:     idToken,
				},
			},
			IdpInformation: intent.IDPUser,
		},
	}, nil
}

func (s *Server) checkIntentToken(token string, intentID string) error {
	if token == "" {
		return errors.ThrowPermissionDenied(nil, "IDP-Sfefs", "Errors.Intent.InvalidToken")
	}
	data, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return errors.ThrowPermissionDenied(err, "IDP-Swg31", "Errors.Intent.InvalidToken")
	}
	decryptedToken, err := s.idpAlg.Decrypt(data, s.idpAlg.EncryptionKeyID())
	if err != nil {
		return errors.ThrowPermissionDenied(err, "IDP-Sf4gt", "Errors.Intent.InvalidToken")
	}
	if string(decryptedToken) != intentID {
		return errors.ThrowPermissionDenied(nil, "IDP-dkje3", "Errors.Intent.InvalidToken")
	}
	return nil
}