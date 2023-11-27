// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package okta

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	oktaold "github.com/chrismalek/oktasdk-go/okta"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/lf-edge/openbao/sdk/framework"
	"github.com/lf-edge/openbao/sdk/helper/tokenutil"
	"github.com/lf-edge/openbao/sdk/logical"
	oktanew "github.com/okta/okta-sdk-golang/v2/okta"
)

const (
	defaultBaseURL = "okta.com"
	previewBaseURL = "oktapreview.com"
)

func pathConfig(b *backend) *framework.Path {
	p := &framework.Path{
		Pattern: `config`,

		DisplayAttrs: &framework.DisplayAttributes{
			OperationPrefix: operationPrefixOkta,
			Action:          "Configure",
		},

		Fields: map[string]*framework.FieldSchema{
			"organization": {
				Type:        framework.TypeString,
				Description: "Use org_name instead.",
				Deprecated:  true,
			},
			"org_name": {
				Type:        framework.TypeString,
				Description: "Name of the organization to be used in the Okta API.",
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "Organization Name",
				},
			},
			"token": {
				Type:        framework.TypeString,
				Description: "Use api_token instead.",
				Deprecated:  true,
			},
			"api_token": {
				Type:        framework.TypeString,
				Description: "Okta API key.",
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "API Token",
				},
			},
			"base_url": {
				Type:        framework.TypeString,
				Description: `The base domain to use for the Okta API. When not specified in the configuration, "okta.com" is used.`,
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "Base URL",
				},
			},
			"production": {
				Type:        framework.TypeBool,
				Description: `Use base_url instead.`,
				Deprecated:  true,
			},
			"ttl": {
				Type:        framework.TypeDurationSecond,
				Description: tokenutil.DeprecationText("token_ttl"),
				Deprecated:  true,
			},
			"max_ttl": {
				Type:        framework.TypeDurationSecond,
				Description: tokenutil.DeprecationText("token_max_ttl"),
				Deprecated:  true,
			},
			"bypass_okta_mfa": {
				Type:        framework.TypeBool,
				Description: `When set true, requests by Okta for a MFA check will be bypassed. This also disallows certain status checks on the account, such as whether the password is expired.`,
				DisplayAttrs: &framework.DisplayAttributes{
					Name: "Bypass Okta MFA",
				},
			},
		},

		Operations: map[logical.Operation]framework.OperationHandler{
			logical.ReadOperation: &framework.PathOperation{
				Callback: b.pathConfigRead,
				DisplayAttrs: &framework.DisplayAttributes{
					OperationSuffix: "configuration",
				},
			},
			logical.CreateOperation: &framework.PathOperation{
				Callback: b.pathConfigWrite,
				DisplayAttrs: &framework.DisplayAttributes{
					OperationVerb: "configure",
				},
			},
			logical.UpdateOperation: &framework.PathOperation{
				Callback: b.pathConfigWrite,
				DisplayAttrs: &framework.DisplayAttributes{
					OperationVerb: "configure",
				},
			},
		},

		ExistenceCheck: b.pathConfigExistenceCheck,

		HelpSynopsis: pathConfigHelp,
	}

	tokenutil.AddTokenFields(p.Fields)
	p.Fields["token_policies"].Description += ". This will apply to all tokens generated by this auth method, in addition to any configured for specific users/groups."
	return p
}

// Config returns the configuration for this backend.
func (b *backend) Config(ctx context.Context, s logical.Storage) (*ConfigEntry, error) {
	entry, err := s.Get(ctx, "config")
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, nil
	}

	var result ConfigEntry
	if entry != nil {
		if err := entry.DecodeJSON(&result); err != nil {
			return nil, err
		}
	}

	if result.TokenTTL == 0 && result.TTL > 0 {
		result.TokenTTL = result.TTL
	}
	if result.TokenMaxTTL == 0 && result.MaxTTL > 0 {
		result.TokenMaxTTL = result.MaxTTL
	}

	return &result, nil
}

func (b *backend) pathConfigRead(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	cfg, err := b.Config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}

	data := map[string]interface{}{
		"organization":    cfg.Org,
		"org_name":        cfg.Org,
		"bypass_okta_mfa": cfg.BypassOktaMFA,
	}
	cfg.PopulateTokenData(data)

	if cfg.BaseURL != "" {
		data["base_url"] = cfg.BaseURL
	}
	if cfg.Production != nil {
		data["production"] = *cfg.Production
	}
	if cfg.TTL > 0 {
		data["ttl"] = int64(cfg.TTL.Seconds())
	}
	if cfg.MaxTTL > 0 {
		data["max_ttl"] = int64(cfg.MaxTTL.Seconds())
	}

	resp := &logical.Response{
		Data: data,
	}

	if cfg.BypassOktaMFA {
		resp.AddWarning("Okta MFA bypass is configured. In addition to ignoring Okta MFA requests, certain other account statuses will not be seen, such as PASSWORD_EXPIRED. Authentication will succeed in these cases.")
	}

	return resp, nil
}

func (b *backend) pathConfigWrite(ctx context.Context, req *logical.Request, d *framework.FieldData) (*logical.Response, error) {
	cfg, err := b.Config(ctx, req.Storage)
	if err != nil {
		return nil, err
	}

	// Due to the existence check, entry will only be nil if it's a create
	// operation, so just create a new one
	if cfg == nil {
		cfg = &ConfigEntry{}
	}

	org, ok := d.GetOk("org_name")
	if ok {
		cfg.Org = org.(string)
	}
	if cfg.Org == "" {
		org, ok = d.GetOk("organization")
		if ok {
			cfg.Org = org.(string)
		}
	}
	if cfg.Org == "" && req.Operation == logical.CreateOperation {
		return logical.ErrorResponse("org_name is missing"), nil
	}

	token, ok := d.GetOk("api_token")
	if ok {
		cfg.Token = token.(string)
	} else if token, ok = d.GetOk("token"); ok {
		cfg.Token = token.(string)
	}

	baseURLRaw, ok := d.GetOk("base_url")
	if ok {
		baseURL := baseURLRaw.(string)
		_, err = url.Parse(fmt.Sprintf("https://%s,%s", cfg.Org, baseURL))
		if err != nil {
			return logical.ErrorResponse(fmt.Sprintf("Error parsing given base_url: %s", err)), nil
		}
		cfg.BaseURL = baseURL
	}

	// We only care about the production flag when base_url is not set. It is
	// for compatibility reasons.
	if cfg.BaseURL == "" {
		productionRaw, ok := d.GetOk("production")
		if ok {
			production := productionRaw.(bool)
			cfg.Production = &production
		}
	} else {
		// clear out old production flag if base_url is set
		cfg.Production = nil
	}

	bypass, ok := d.GetOk("bypass_okta_mfa")
	if ok {
		cfg.BypassOktaMFA = bypass.(bool)
	}

	if err := cfg.ParseTokenFields(req, d); err != nil {
		return logical.ErrorResponse(err.Error()), logical.ErrInvalidRequest
	}

	// Handle upgrade cases
	{
		if err := tokenutil.UpgradeValue(d, "ttl", "token_ttl", &cfg.TTL, &cfg.TokenTTL); err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}

		if err := tokenutil.UpgradeValue(d, "max_ttl", "token_max_ttl", &cfg.MaxTTL, &cfg.TokenMaxTTL); err != nil {
			return logical.ErrorResponse(err.Error()), nil
		}
	}

	jsonCfg, err := logical.StorageEntryJSON("config", cfg)
	if err != nil {
		return nil, err
	}
	if err := req.Storage.Put(ctx, jsonCfg); err != nil {
		return nil, err
	}

	var resp *logical.Response
	if cfg.BypassOktaMFA {
		resp = new(logical.Response)
		resp.AddWarning("Okta MFA bypass is configured. In addition to ignoring Okta MFA requests, certain other account statuses will not be seen, such as PASSWORD_EXPIRED. Authentication will succeed in these cases.")
	}

	return resp, nil
}

func (b *backend) pathConfigExistenceCheck(ctx context.Context, req *logical.Request, d *framework.FieldData) (bool, error) {
	cfg, err := b.Config(ctx, req.Storage)
	if err != nil {
		return false, err
	}

	return cfg != nil, nil
}

type oktaShim interface {
	Client() (*oktanew.Client, context.Context)
	NewRequest(method string, url string, body interface{}) (*http.Request, error)
	Do(req *http.Request, v interface{}) (interface{}, error)
}

type oktaShimNew struct {
	client *oktanew.Client
	ctx    context.Context
}

func (new *oktaShimNew) Client() (*oktanew.Client, context.Context) {
	return new.client, new.ctx
}

func (new *oktaShimNew) NewRequest(method string, url string, body interface{}) (*http.Request, error) {
	if !strings.HasPrefix(url, "/") {
		url = "/api/v1/" + url
	}
	return new.client.GetRequestExecutor().NewRequest(method, url, body)
}

func (new *oktaShimNew) Do(req *http.Request, v interface{}) (interface{}, error) {
	return new.client.GetRequestExecutor().Do(new.ctx, req, v)
}

type oktaShimOld struct {
	client *oktaold.Client
}

func (new *oktaShimOld) Client() (*oktanew.Client, context.Context) {
	return nil, nil
}

func (new *oktaShimOld) NewRequest(method string, url string, body interface{}) (*http.Request, error) {
	return new.client.NewRequest(method, url, body)
}

func (new *oktaShimOld) Do(req *http.Request, v interface{}) (interface{}, error) {
	return new.client.Do(req, v)
}

// OktaClient creates a basic okta client connection
func (c *ConfigEntry) OktaClient(ctx context.Context) (oktaShim, error) {
	baseURL := defaultBaseURL
	if c.Production != nil {
		if !*c.Production {
			baseURL = previewBaseURL
		}
	}
	if c.BaseURL != "" {
		baseURL = c.BaseURL
	}

	if c.Token != "" {
		ctx, client, err := oktanew.NewClient(ctx,
			oktanew.WithOrgUrl("https://"+c.Org+"."+baseURL),
			oktanew.WithToken(c.Token))
		if err != nil {
			return nil, err
		}
		return &oktaShimNew{client, ctx}, nil
	}
	client, err := oktaold.NewClientWithDomain(cleanhttp.DefaultClient(), c.Org, baseURL, "")
	if err != nil {
		return nil, err
	}
	return &oktaShimOld{client}, nil
}

// ConfigEntry for Okta
type ConfigEntry struct {
	tokenutil.TokenParams

	Org           string        `json:"organization"`
	Token         string        `json:"token"`
	BaseURL       string        `json:"base_url"`
	Production    *bool         `json:"is_production,omitempty"`
	TTL           time.Duration `json:"ttl"`
	MaxTTL        time.Duration `json:"max_ttl"`
	BypassOktaMFA bool          `json:"bypass_okta_mfa"`
}

const pathConfigHelp = `
This endpoint allows you to configure the Okta and its
configuration options.

The Okta organization are the characters at the front of the URL for Okta.
Example https://ORG.okta.com
`
