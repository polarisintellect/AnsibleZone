// Copyright 2017 Istio Authors
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

// Routing tests

package main

import (
	"fmt"
	"strings"
	"time"
	// TODO(nmittler): Remove this
	_ "github.com/golang/glog"
	multierror "github.com/hashicorp/go-multierror"

	"istio.io/istio/pkg/log"
)

type egressRules struct {
	*infra
}

func (t *egressRules) String() string {
	return "egress-rules"
}

func (t *egressRules) setup() error {
	return nil
}

// TODO: test negatives
func (t *egressRules) run() error {
	cases := []struct {
		description string
		config      string
		check       func() error
	}{
		{
			description: "allow external traffic to httbin.org",
			config:      "egress-rule-httpbin.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("http://httpbin.org/headers", true)
			},
		},
		{
			description: "allow external traffic to *.httbin.org",
			config:      "egress-rule-wildcard-httpbin.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("http://www.httpbin.org/headers", true)
			},
		},
		{
			description: "ensure traffic to httbin.org is prohibited when setting *.httbin.org",
			config:      "egress-rule-wildcard-httpbin.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("http://httpbin.org/headers", false)
			},
		},
		{
			description: "allow external http2 traffic to nghttp2.org",
			config:      "egress-rule-nghttp2.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("http://nghttp2.org", true)
			},
		},
		{
			description: "prohibit https to httbin.org",
			config:      "egress-rule-httpbin.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("http://httpbin.org:443/headers", false)
			},
		},
		{
			description: "allow https external traffic to www.wikipedia.org by a tcp egress rule with cidr",
			config:      "egress-rule-tcp-wikipedia-cidr.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("https://www.wikipedia.org", true)
			},
		},
		{
			description: "prohibit http external traffic to cnn.com by a tcp egress rule",
			config:      "egress-rule-tcp-wikipedia-cidr.yaml.tmpl",
			check: func() error {
				return t.verifyReachable("https://cnn.com", false)
			},
		},
	}
	var errs error
	for _, cs := range cases {
		tlog("Checking egressRules test", cs.description)
		if err := t.applyConfig(cs.config, nil); err != nil {
			return err
		}

		if err := repeat(cs.check, 3, time.Second); err != nil {
			log.Infof("Failed the test with %v", err)
			errs = multierror.Append(errs, multierror.Prefix(err, cs.description))
		} else {
			log.Info("Success!")
		}

		if err := t.deleteConfig(cs.config, nil); err != nil {
			return err
		}
	}
	return errs
}

func (t *egressRules) teardown() {
	log.Info("Cleaning up egress rules...")
	if err := t.deleteAllConfigs(); err != nil {
		log.Warna(err)
	}
}

// verifyReachable verifies that the url is reachable
func (t *egressRules) verifyReachable(url string, shouldBeReachable bool) error {
	funcs := make(map[string]func() status)
	for _, src := range []string{"a", "b"} {
		name := fmt.Sprintf("Request from %s to %s", src, url)
		funcs[name] = (func(src string) func() status {
			trace := fmt.Sprint(time.Now().UnixNano())
			return func() status {
				resp := t.clientRequest(src, url, 1, fmt.Sprintf("-key Trace-Id -val %q", trace))
				reachable := len(resp.code) > 0 && resp.code[0] == httpOk && strings.Contains(resp.body, trace)
				if reachable && !shouldBeReachable {
					return fmt.Errorf("%s is reachable from %s (should be unreachable)", url, src)
				}
				if !reachable && shouldBeReachable {
					return errAgain
				}

				return nil
			}
		})(src)
	}

	return parallel(funcs)
}
