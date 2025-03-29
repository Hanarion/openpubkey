// Copyright 2025 OpenPubkey
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package choosers

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	"github.com/openpubkey/openpubkey/providers"
	"github.com/openpubkey/openpubkey/util"
	"github.com/sirupsen/logrus"
)

//go:embed static/*
var staticFiles embed.FS

//go:embed chooser.tmpl
var chooserTemplateFile string

// To add support for an OP to the the WebChooser:
// 1. Add the OP to IssuerToName func
// 2. Add the OP to the html template file: `chooser.tmpl`
// 3. Add the OP to the data which is supplied to `chooserTemplate.Execute(w, data)`
//
// Note that the web chooser can only support BrowserOpenIdProvider

// TODO: This should be an enum that can also autogenerate what gets passed to the template

type WebChooser struct {
	OpList        []providers.BrowserOpenIdProvider
	opSelected    providers.BrowserOpenIdProvider
	OpenBrowser   bool
	useMockServer bool
	mockServer    *httptest.Server
	server        *http.Server
}

func NewWebChooser(opList []providers.BrowserOpenIdProvider) *WebChooser {
	return &WebChooser{
		OpList:        opList,
		OpenBrowser:   true,
		useMockServer: false,
	}
}

func (wc *WebChooser) ChooseOp(ctx context.Context) (providers.OpenIdProvider, error) {
	if wc.opSelected != nil {
		return nil, fmt.Errorf("provider has already been chosen")
	}

	providerMap := map[string]providers.BrowserOpenIdProvider{}
	for _, provider := range wc.OpList {
		if providers.CheckURLStatus(provider.Issuer() + "/.well-known/openid-configuration") {
			if providerName, err := IssuerToName(provider.Issuer()); err != nil {
				return nil, err
			} else {
				if _, ok := providerMap[providerName]; ok {
					return nil, fmt.Errorf("provider in web chooser found with duplicate issuer: %s", provider.Issuer())
				}
				providerMap[providerName] = provider
			}
		} else {
			return nil, fmt.Errorf("provider issuer is not a valid openid issuer: %s", provider.Issuer()+"/.well-known/openid-configuration not found")
		}
	}

	opCh := make(chan providers.BrowserOpenIdProvider, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	staticContent, err := fs.Sub(staticFiles, "static")
	if err != nil {
		return nil, err
	}

	chooserTemplate, err := template.New("chooser-page").Parse(chooserTemplateFile)
	if err != nil {
		return nil, err
	}

	mux.HandleFunc("/chooser", func(w http.ResponseWriter, r *http.Request) {
		type Provider struct {
			Name   string
			Button string
		}

		data := struct {
			Providers []Provider
		}{}

		sortedProviderNames := make([]string, 0)
		for providerName := range providerMap {
			sortedProviderNames = append(sortedProviderNames, providerName)
		}
		// Sort the provider names
		sort.Strings(sortedProviderNames)

		for _, providerName := range sortedProviderNames {
			if providerName == "google" {
				data.Providers = append(data.Providers, Provider{
					Name:   providerName,
					Button: "google-light.svg",
				})
				continue
			}
			if providerName == "azure" {
				data.Providers = append(data.Providers, Provider{
					Name:   providerName,
					Button: "azure-dark.svg",
				})
				continue
			}
			if providerName == "gitlab" {
				data.Providers = append(data.Providers, Provider{
					Name:   providerName,
					Button: "gitlab-light.svg",
				})
				continue
			}
			data.Providers = append(data.Providers, Provider{
				Name:   providerName,
				Button: "",
			})

		}

		w.Header().Set("Content-Type", "text/html")
		if err := chooserTemplate.Execute(w, data); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticContent))))
	mux.HandleFunc("/select/", func(w http.ResponseWriter, r *http.Request) {
		// Once we redirect to the OP localhost webserver, we can shutdown the web chooser localhost server
		shutdownServer := func() {
			go func() { // Put this in a go func so that it will not block the redirect
				if wc.server != nil {
					if err := wc.server.Shutdown(ctx); err != nil {
						logrus.Errorf("Failed to shutdown http server: %v", err)
					}
				}
			}()
		}
		defer shutdownServer()

		opName := r.URL.Query().Get("op")
		if opName == "" {
			errorString := "missing op parameter"
			http.Error(w, errorString, http.StatusBadRequest)
			errCh <- errors.New(errorString)
			return
		}
		if op, ok := providerMap[opName]; !ok {
			errorString := fmt.Sprintf("unknown OpenID Provider: %s", opName)
			http.Error(w, errorString, http.StatusBadRequest)
			errCh <- errors.New(errorString)
			return
		} else {
			opCh <- op

			redirectUriCh := make(chan string, 1)
			op.ReuseBrowserWindowHook(redirectUriCh)

			redirectUri := <-redirectUriCh
			http.Redirect(w, r, redirectUri, http.StatusFound)
		}
	})

	if wc.useMockServer {
		wc.mockServer = httptest.NewUnstartedServer(mux)
		wc.mockServer.Start()
	} else {
		listener, err := net.Listen("tcp", "localhost:0")
		if err != nil {
			return nil, fmt.Errorf("failed to bind to an available port: %w", err)
		}
		wc.server = &http.Server{Handler: mux}
		go func() {
			err = wc.server.Serve(listener)
			if err != nil && err != http.ErrServerClosed {
				logrus.Error(err)
			}
		}()

		if wc.OpenBrowser {
			loginURI := fmt.Sprintf("http://%s/chooser", listener.Addr().String())
			logrus.Infof("Opening browser to %s", loginURI)
			if err := util.OpenUrl(loginURI); err != nil {
				logrus.Errorf("Failed to open url: %v", err)
			}
		}
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-errCh:
		return nil, err
	case wc.opSelected = <-opCh:
		return wc.opSelected, nil
	}
}

func IssuerToName(issuer string) (string, error) {
	switch {
	case strings.HasPrefix(issuer, "https://accounts.google.com"):
		return "google", nil
	case strings.HasPrefix(issuer, "https://login.microsoftonline.com"):
		return "azure", nil
	case strings.HasPrefix(issuer, "https://gitlab.com"):
		return "gitlab", nil
	default:
		if strings.HasPrefix(issuer, "https://") {
			// Returns issuer without the "https://" prefix and without any path remaining on the url
			// e.g. https://accounts.google.com/fdsfa/fdsafsad -> accounts.google.com
			return strings.Split(strings.TrimPrefix(issuer, "https://"), "/")[0], nil

		}
		return "", fmt.Errorf("invalid OpenID Provider issuer: %s", issuer)
	}
}
