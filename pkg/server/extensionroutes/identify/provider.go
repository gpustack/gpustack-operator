package identify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"unicode"

	dexconnector "github.com/dexidp/dex/connector"
	"github.com/dexidp/dex/connector/bitbucketcloud"
	"github.com/dexidp/dex/connector/gitea"
	"github.com/dexidp/dex/connector/github"
	"github.com/dexidp/dex/connector/gitlab"
	"github.com/dexidp/dex/connector/google"
	"github.com/dexidp/dex/connector/ldap"
	"github.com/dexidp/dex/connector/microsoft"
	"github.com/dexidp/dex/connector/oauth"
	"github.com/dexidp/dex/connector/oidc"
	dexserver "github.com/dexidp/dex/server"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	klog "k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

type (
	ExternalIdentity          = dexconnector.Identity
	ExternalConnectorConfig   = dexserver.ConnectorConfig
	ExternalConnector         = dexconnector.Connector
	ExternalPasswordConnector interface {
		// Login logs in with the given username and password.
		// It returns the identity if the login is successful,
		// the valid flag indicates whether the login is valid.
		Login(ctx context.Context, username, password string) (id *ExternalIdentity, valid bool, err error)
	}
	ExternalCallbackConnector interface {
		// GetLoginURL returns the login URL with the given callback URL and state.
		GetLoginURL(callbackUrl, state string) (loginUrl string, err error)
		// GetClientID returns the client ID.
		GetClientID() string
		// HandleCallback handles the callback request and returns the identity,
		// the given callback URL must be the same as the one used in GetLoginURL.
		HandleCallback(callbackUrl string, req *http.Request) (id *ExternalIdentity, err error)
	}
)

type _ExternalPasswordConnector struct {
	ConnConfig ExternalConnectorConfig
}

// NewExternalPasswordConnector creates a new ExternalPasswordConnector.
func NewExternalPasswordConnector(connConfig ExternalConnectorConfig) ExternalPasswordConnector {
	return &_ExternalPasswordConnector{ConnConfig: connConfig}
}

func (p *_ExternalPasswordConnector) openConnector() (dexconnector.PasswordConnector, error) {
	lgh := slog.NewTextHandler(
		klog.Background().WithName("identify"),
		&slog.HandlerOptions{
			Level: slog.LevelInfo,
		},
	)
	conn, err := p.ConnConfig.Open("", slog.New(lgh))
	if err != nil {
		return nil, err
	}
	return conn.(dexconnector.PasswordConnector), nil
}

func (p *_ExternalPasswordConnector) Login(ctx context.Context, username, password string) (*ExternalIdentity, bool, error) {
	conn, err := p.openConnector()
	if err != nil {
		return nil, false, fmt.Errorf("open connector: %w", err)
	}
	id, ok, err := conn.Login(ctx, dexconnector.Scopes{Groups: true, OfflineAccess: true}, username, password)
	if err != nil {
		return nil, false, fmt.Errorf("login failed : %w", err)
	}
	return &id, ok, nil
}

type _ExternalCallbackConnector[T ExternalConnectorConfig] struct {
	ConnConfig        T
	ClientID          string
	InjectCallbackUrl func(callbackUrl string, ConnConfig T)
}

// NewExternalCallbackConnector creates a new ExternalCallbackConnector.
func NewExternalCallbackConnector[T ExternalConnectorConfig](connConfig T, clientID string, injectCallbackUrl func(string, T)) ExternalCallbackConnector { // nolint:lll
	return &_ExternalCallbackConnector[T]{
		ConnConfig:        connConfig,
		ClientID:          clientID,
		InjectCallbackUrl: injectCallbackUrl,
	}
}

func (p *_ExternalCallbackConnector[T]) openConnector() (dexconnector.CallbackConnector, error) {
	lgh := slog.NewTextHandler(
		klog.Background().WithName("identify"),
		&slog.HandlerOptions{
			Level: slog.LevelInfo,
		},
	)
	conn, err := p.ConnConfig.Open("", slog.New(lgh))
	if err != nil {
		return nil, fmt.Errorf("open connection: %w", err)
	}
	return conn.(dexconnector.CallbackConnector), nil
}

func (p *_ExternalCallbackConnector[T]) GetLoginURL(callbackUrl, state string) (string, error) {
	if p.InjectCallbackUrl != nil {
		p.InjectCallbackUrl(callbackUrl, p.ConnConfig)
	}

	conn, err := p.openConnector()
	if err != nil {
		return "", fmt.Errorf("open connector: %w", err)
	}
	u, err := conn.LoginURL(dexconnector.Scopes{Groups: true, OfflineAccess: true}, callbackUrl, state)
	if err != nil {
		return "", fmt.Errorf("get login url failed: %w", err)
	}
	return u, nil
}

func (p *_ExternalCallbackConnector[T]) GetClientID() string {
	return p.ClientID
}

func (p *_ExternalCallbackConnector[T]) HandleCallback(callbackUrl string, req *http.Request) (*ExternalIdentity, error) {
	if p.InjectCallbackUrl != nil {
		p.InjectCallbackUrl(callbackUrl, p.ConnConfig)
	}

	conn, err := p.openConnector()
	if err != nil {
		return nil, fmt.Errorf("open connector: %w", err)
	}
	id, err := conn.HandleCallback(dexconnector.Scopes{Groups: true, OfflineAccess: true}, req)
	if err != nil {
		return nil, fmt.Errorf("handle callback failed: %w", err)
	}
	return &id, nil
}

// getExternalConnectorFromSubjectProvider converts a GPUStack SubjectProvider to ExternalConnector.
func getExternalConnectorFromSubjectProvider(subjProv *server.SubjectProvider) (ExternalConnector, error) {
	if subjProv == nil {
		return nil, errors.New("provider is nil")
	}

	if subjProv.Spec.Type == server.SubjectProviderTypeInternal {
		return nil, errors.New("internal provider is not supported")
	}

	err := subjProv.Spec.ExternalConfig.ValidateWithType(subjProv.Spec.Type)
	if err != nil {
		return nil, fmt.Errorf("provider: invalid external config: %w", err)
	}

	switch subjProv.Spec.Type {
	default:
		return nil, fmt.Errorf("provider: unsupported type: %s", subjProv.Spec.Type)
	case server.SubjectProviderTypeLDAP:
		src := subjProv.Spec.ExternalConfig.LDAP
		dst := &ldap.Config{
			InsecureSkipVerify: src.SkipInsecureVerify,
			Host:               src.Host,
			BindDN:             src.ServiceAccountDN,
			BindPW:             src.ServiceAccountPassword,
		}
		if _, port, _ := net.SplitHostPort(src.Host); port == "389" {
			dst.InsecureNoSSL = true
		}
		dst.UserSearch.BaseDN = src.UserSearch.BaseDN
		dst.UserSearch.Filter = src.UserSearch.Filter
		dst.UserSearch.Username = src.UserSearch.NameAttribute
		dst.UserSearch.Scope = "sub"
		dst.UserSearch.IDAttr = "uid"
		dst.UserSearch.EmailAttr = src.UserSearch.EmailAttribute
		dst.UserSearch.NameAttr = src.UserSearch.NameAttribute
		dst.UserSearch.PreferredUsernameAttrAttr = src.UserSearch.DisplayNameAttribute
		dst.GroupSearch.BaseDN = src.GroupSearch.BaseDN
		dst.GroupSearch.Filter = src.GroupSearch.Filter
		dst.GroupSearch.Scope = "sub"
		for _, um := range src.GroupSearch.UserMatchers {
			dst.GroupSearch.UserMatchers = append(dst.GroupSearch.UserMatchers,
				ldap.UserMatcher{
					UserAttr:  um.UserAttribute,
					GroupAttr: um.GroupAttribute,
				})
		}
		dst.GroupSearch.NameAttr = src.GroupSearch.NameAttribute
		return NewExternalPasswordConnector(dst), nil
	case server.SubjectProviderTypeOAuth:
		src := subjProv.Spec.ExternalConfig.OAuth
		dst := &oauth.Config{
			InsecureSkipVerify: src.SkipInsecureVerify,
			ClientID:           src.ClientID,
			ClientSecret:       src.ClientSecret,
			TokenURL:           src.TokenEndpoint,
			AuthorizationURL:   src.AuthorizationEndpoint,
			UserInfoURL:        src.UserinfoEndpoint,
		}
		if cm := src.ClaimMapping; cm != nil {
			dst.ClaimMapping.UserNameKey = cm.NameKey
			dst.ClaimMapping.PreferredUsernameKey = cm.DisplayNameKey
			dst.ClaimMapping.EmailKey = cm.EmailKey
			dst.ClaimMapping.GroupsKey = cm.GroupsKey
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *oauth.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeOIDC:
		src := subjProv.Spec.ExternalConfig.OIDC
		dst := &oidc.Config{
			InsecureSkipVerify:   src.SkipInsecureVerify,
			Issuer:               src.Issuer,
			ClientID:             src.ClientID,
			ClientSecret:         src.ClientSecret,
			InsecureEnableGroups: true,
			PromptType:           ptr.To("consent"),
		}
		if cm := src.ClaimMapping; cm != nil {
			dst.UserNameKey = cm.NameKey
			dst.ClaimMapping.PreferredUsernameKey = cm.DisplayNameKey
			dst.ClaimMapping.EmailKey = cm.EmailKey
			dst.ClaimMapping.GroupsKey = cm.GroupsKey
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *oidc.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeGitHub:
		src := subjProv.Spec.ExternalConfig.GitHub
		dst := &github.Config{
			ClientID:      src.ClientID,
			ClientSecret:  src.ClientSecret,
			TeamNameField: "both",
			LoadAllGroups: true,
			UseLoginAsID:  true,
		}
		for k, v := range src.Groups.ToMap() {
			dst.Orgs = append(dst.Orgs, github.Org{
				Name:  k,
				Teams: v,
			})
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *github.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeGitLab:
		src := subjProv.Spec.ExternalConfig.GitLab
		dst := &gitlab.Config{
			ClientID:     src.ClientID,
			ClientSecret: src.ClientSecret,
			Groups:       src.Groups,
			UseLoginAsID: true,
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *gitlab.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeBitbucket:
		src := subjProv.Spec.ExternalConfig.Bitbucket
		dst := &bitbucketcloud.Config{
			ClientID:          src.ClientID,
			ClientSecret:      src.ClientSecret,
			Teams:             src.Groups,
			IncludeTeamGroups: true,
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *bitbucketcloud.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeGitea:
		src := subjProv.Spec.ExternalConfig.Gitea
		dst := &gitea.Config{
			ClientID:      src.ClientID,
			ClientSecret:  src.ClientSecret,
			LoadAllGroups: len(src.Groups) == 0,
			UseLoginAsID:  true,
		}
		for k, v := range src.Groups.ToMap() {
			dst.Orgs = append(dst.Orgs, gitea.Org{
				Name:  k,
				Teams: v,
			})
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *gitea.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeGoogle:
		src := subjProv.Spec.ExternalConfig.Google
		dst := &google.Config{
			ClientID:     src.ClientID,
			ClientSecret: src.ClientSecret,
			Groups:       src.Groups,
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *google.Config) { o.RedirectURI = s }), nil
	case server.SubjectProviderTypeMicrosoft:
		src := subjProv.Spec.ExternalConfig.Microsoft
		dst := &microsoft.Config{
			ClientID:         src.ClientID,
			ClientSecret:     src.ClientSecret,
			Groups:           src.Groups,
			Tenant:           src.Tenant.String(),
			GroupNameFormat:  microsoft.GroupName,
			EmailToLowercase: true,
		}
		return NewExternalCallbackConnector(dst, src.ClientID,
			func(s string, o *microsoft.Config) { o.RedirectURI = s }), nil
	}
}

// convertSubjectFromExternalIdentity converts an ExternalIdentity to GPUStack subject.
func convertSubjectFromExternalIdentity(ctx context.Context, provider string, id *ExternalIdentity) (*server.Subject, error) {
	if id.PreferredUsername == "" && id.Username == "" {
		return nil, errors.New("both name and display name are empty")
	}

	sort.Strings(id.Groups)

	// Normalize names.
	var name, displayName string
	{
		name = id.PreferredUsername
		displayName = id.Username
		switch {
		case name == "":
			name = displayName
		case displayName == "":
			displayName = name
		}
		// Take the shortest name for constructing the subject name.
		if stringx.StringWidth(name) > stringx.StringWidth(displayName) {
			name, displayName = displayName, name
		}
		name = strings.TrimSpace(strings.ToLower(name))
		name = stringx.ReplaceFunc(name, func(r rune) rune {
			if r == '.' || r == '-' || unicode.IsOneOf(
				[]*unicode.RangeTable{unicode.Number, unicode.Letter}, r) {
				return r
			}
			return '-'
		})
	}

	// Create or update.
	eSubj := &server.Subject{
		ObjectMeta: meta.ObjectMeta{
			Namespace: kuberess.SystemNamespaceName,
			Name:      stringx.Join(".", provider, name), // NB(thxCode): make sure the name is unique.
		},
		Spec: server.SubjectSpec{
			Provider:    provider,
			Role:        server.SubjectRoleUser,
			DisplayName: displayName,
			Description: "Login from provider",
			Email:       id.Email,
			Groups:      id.Groups,
			Credential:  ptr.To(stringx.SumBytesBySHA256(id.ConnectorData)),
		},
	}
	alignFn := func(aSubj *server.Subject) (*server.Subject, bool, error) {
		aSubj.Spec.Groups = eSubj.Spec.Groups
		aSubj.Spec.Credential = eSubj.Spec.Credential
		return aSubj, false, nil
	}
	subj, err := kubeclientset.UpdateWithCtrlClient(ctx, system.LoopbackCtrlClient.Get(), eSubj,
		kubeclientset.WithUpdateAlign(alignFn),
		kubeclientset.WithCreateIfNotExisted[*server.Subject]())
	if err != nil {
		return nil, err
	}

	// Since the credential is a write-only field, it is not returned.
	// We need to copy the credential back.
	subj.Spec.Credential = eSubj.Spec.Credential
	return subj, nil
}
