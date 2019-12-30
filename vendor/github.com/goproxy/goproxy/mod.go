package goproxy

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

// modResult is an unified result of the `mod`.
type modResult struct {
	Version  string
	Versions []string
	Info     string
	GoMod    string
	Zip      string
}

// mod executes the Go modules related commands based on the operation.
func mod(
	operation string,
	httpClient *http.Client,
	goBinName string,
	goBinEnv map[string]string,
	goBinWorkerChan chan struct{},
	goproxyRoot string,
	modulePath string,
	moduleVersion string,
) (*modResult, error) {
	switch operation {
	case "lookup", "latest", "list", "download":
	default:
		return nil, errors.New("invalid mod operation")
	}

	// Try proxies.

	escapedModulePath, err := module.EscapePath(modulePath)
	if err != nil {
		return nil, err
	}

	escapedModuleVersion, err := module.EscapeVersion(moduleVersion)
	if err != nil {
		return nil, err
	}

	var (
		tryDirect    bool
		proxies      []string
		lastNotFound string
	)

	if globsMatchPath(goBinEnv["GONOPROXY"], modulePath) {
		tryDirect = true
	} else {
		proxies = strings.Split(goBinEnv["GOPROXY"], ",")
	}

	for _, proxy := range proxies {
		if proxy == "direct" {
			tryDirect = true
			break
		}

		if proxy == "off" {
			return nil, errors.New("disabled by GOPROXY=off")
		}

		proxyURL, err := parseRawURL(proxy)
		if err != nil {
			return nil, err
		}

		switch operation {
		case "lookup", "latest":
			var operationURL *url.URL
			if operation == "lookup" {
				operationURL = appendURL(
					proxyURL,
					escapedModulePath,
					"@v",
					fmt.Sprint(
						escapedModuleVersion,
						".info",
					),
				)
			} else {
				operationURL = appendURL(
					proxyURL,
					escapedModulePath,
					"@latest",
				)
			}

			res, err := httpClient.Get(operationURL.String())
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()

			b, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return nil, err
			}

			switch res.StatusCode {
			case http.StatusOK:
			case http.StatusBadRequest:
				return nil, fmt.Errorf("%s", b)
			case http.StatusNotFound, http.StatusGone:
				lastNotFound = string(b)
				continue
			default:
				return nil, fmt.Errorf(
					"GET %s: %s: %s",
					redactedURL(operationURL),
					res.Status,
					b,
				)
			}

			mr := modResult{}
			if err := json.Unmarshal(b, &mr); err != nil {
				return nil, err
			}

			return &mr, nil
		case "list":
			operationURL := appendURL(
				proxyURL,
				escapedModulePath,
				"@v",
				"list",
			)

			res, err := httpClient.Get(operationURL.String())
			if err != nil {
				return nil, err
			}
			defer res.Body.Close()

			b, err := ioutil.ReadAll(res.Body)
			if err != nil {
				return nil, err
			}

			switch res.StatusCode {
			case http.StatusOK:
			case http.StatusBadRequest:
				return nil, fmt.Errorf("%s", b)
			case http.StatusNotFound, http.StatusGone:
				lastNotFound = string(b)
				continue
			default:
				return nil, fmt.Errorf(
					"GET %s: %s: %s",
					redactedURL(operationURL),
					res.Status,
					b,
				)
			}

			versions := []string{}
			for _, b := range bytes.Split(b, []byte{'\n'}) {
				if len(b) == 0 {
					continue
				}

				versions = append(versions, string(b))
			}

			sort.Slice(versions, func(i, j int) bool {
				return semver.Compare(
					versions[i],
					versions[j],
				) < 0
			})

			return &modResult{
				Versions: versions,
			}, nil
		case "download":
			infoFileURL := appendURL(
				proxyURL,
				escapedModulePath,
				"@v",
				fmt.Sprint(escapedModuleVersion, ".info"),
			)

			infoFileRes, err := httpClient.Get(infoFileURL.String())
			if err != nil {
				return nil, err
			}
			defer infoFileRes.Body.Close()

			if infoFileRes.StatusCode != http.StatusOK {
				b, err := ioutil.ReadAll(infoFileRes.Body)
				if err != nil {
					return nil, err
				}

				switch infoFileRes.StatusCode {
				case http.StatusBadRequest:
					return nil, fmt.Errorf("%s", b)
				case http.StatusNotFound, http.StatusGone:
					lastNotFound = string(b)
					continue
				}

				return nil, fmt.Errorf(
					"GET %s: %s: %s",
					redactedURL(infoFileURL),
					infoFileRes.Status,
					b,
				)
			}

			infoFile, err := ioutil.TempFile(goproxyRoot, "info")
			if err != nil {
				return nil, err
			}

			if _, err := io.Copy(
				infoFile,
				infoFileRes.Body,
			); err != nil {
				return nil, err
			}

			if err := infoFile.Close(); err != nil {
				return nil, err
			}

			if err := checkInfoFile(infoFile.Name()); err != nil {
				return nil, err
			}

			modFileURL := appendURL(
				proxyURL,
				escapedModulePath,
				"@v",
				fmt.Sprint(escapedModuleVersion, ".mod"),
			)

			modFileRes, err := httpClient.Get(modFileURL.String())
			if err != nil {
				return nil, err
			}
			defer modFileRes.Body.Close()

			if modFileRes.StatusCode != http.StatusOK {
				b, err := ioutil.ReadAll(modFileRes.Body)
				if err != nil {
					return nil, err
				}

				switch modFileRes.StatusCode {
				case http.StatusBadRequest:
					return nil, fmt.Errorf("%s", b)
				case http.StatusNotFound, http.StatusGone:
					lastNotFound = string(b)
					continue
				}

				return nil, fmt.Errorf(
					"GET %s: %s: %s",
					redactedURL(modFileURL),
					modFileRes.Status,
					b,
				)
			}

			modFile, err := ioutil.TempFile(goproxyRoot, "mod")
			if err != nil {
				return nil, err
			}

			if _, err := io.Copy(
				modFile,
				modFileRes.Body,
			); err != nil {
				return nil, err
			}

			if err := modFile.Close(); err != nil {
				return nil, err
			}

			if err := checkModFile(modFile.Name()); err != nil {
				return nil, err
			}

			zipFileURL := appendURL(
				proxyURL,
				escapedModulePath,
				"@v",
				fmt.Sprint(escapedModuleVersion, ".zip"),
			)

			zipFileRes, err := httpClient.Get(zipFileURL.String())
			if err != nil {
				return nil, err
			}
			defer zipFileRes.Body.Close()

			if zipFileRes.StatusCode != http.StatusOK {
				b, err := ioutil.ReadAll(zipFileRes.Body)
				if err != nil {
					return nil, err
				}

				switch zipFileRes.StatusCode {
				case http.StatusBadRequest:
					return nil, fmt.Errorf("%s", b)
				case http.StatusNotFound, http.StatusGone:
					lastNotFound = string(b)
					continue
				}

				return nil, fmt.Errorf(
					"GET %s: %s: %s",
					redactedURL(zipFileURL),
					zipFileRes.Status,
					b,
				)
			}

			zipFile, err := ioutil.TempFile(goproxyRoot, "zip")
			if err != nil {
				return nil, err
			}

			if _, err := io.Copy(
				zipFile,
				zipFileRes.Body,
			); err != nil {
				return nil, err
			}

			if err := zipFile.Close(); err != nil {
				return nil, err
			}

			if err := checkZIPFile(zipFile.Name()); err != nil {
				return nil, err
			}

			return &modResult{
				Info:  infoFile.Name(),
				GoMod: modFile.Name(),
				Zip:   zipFile.Name(),
			}, nil
		}
	}

	if !tryDirect {
		if lastNotFound != "" {
			return nil, errors.New(lastNotFound)
		}

		return nil, fmt.Errorf("unknown revision %s", moduleVersion)
	}

	// Try direct.

	if goBinWorkerChan != nil {
		goBinWorkerChan <- struct{}{}
		defer func() {
			<-goBinWorkerChan
		}()
	}

	var args []string
	switch operation {
	case "lookup", "latest":
		args = []string{
			"list",
			"-json",
			"-m",
			fmt.Sprint(modulePath, "@", moduleVersion),
		}
	case "list":
		args = []string{
			"list",
			"-json",
			"-m",
			"-versions",
			fmt.Sprint(modulePath, "@", moduleVersion),
		}
	case "download":
		args = []string{
			"mod",
			"download",
			"-json",
			fmt.Sprint(modulePath, "@", moduleVersion),
		}
	}

	cmd := exec.Command(goBinName, args...)
	cmd.Env = make([]string, 0, len(goBinEnv)+9)
	for k, v := range goBinEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd.Env = append(
		cmd.Env,
		fmt.Sprint("GOPATH=", goproxyRoot),
		fmt.Sprint("GOCACHE=", goproxyRoot),
		fmt.Sprint("GOTMPDIR=", goproxyRoot),
		"GO111MODULE=on",
		"GOPROXY=direct",
		"GONOPROXY=",
		"GOSUMDB=off",
		"GONOSUMDB=",
		"GOPRIVATE=",
	)

	cmd.Dir = goproxyRoot
	stdout, err := cmd.Output()
	if err != nil {
		output := stdout
		if len(output) > 0 {
			m := map[string]interface{}{}
			if err := json.Unmarshal(output, &m); err != nil {
				return nil, err
			}

			if es, ok := m["Error"].(string); ok {
				output = []byte(es)
			}
		} else if ee, ok := err.(*exec.ExitError); ok {
			output = ee.Stderr
		}

		var errorMessage string
		for _, line := range strings.Split(string(output), "\n") {
			if strings.HasPrefix(line, "go: finding") {
				continue
			}

			errorMessage = fmt.Sprint(errorMessage, line, "\n")
		}

		errorMessage = strings.TrimPrefix(errorMessage, "go list -m: ")
		errorMessage = strings.TrimRight(errorMessage, "\n")

		return nil, errors.New(errorMessage)
	}

	mr := modResult{}
	if err := json.Unmarshal(stdout, &mr); err != nil {
		return nil, err
	}

	return &mr, nil
}

// modClean cleans the goproxyRoot.
func modClean(
	goBinName string,
	goBinEnv map[string]string,
	goproxyRoot string,
) error {
	cmd := exec.Command(goBinName, "clean", "-modcache")
	cmd.Env = make([]string, 0, len(goBinEnv)+3)
	for k, v := range goBinEnv {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd.Env = append(
		cmd.Env,
		fmt.Sprint("GOPATH=", goproxyRoot),
		fmt.Sprint("GOCACHE=", goproxyRoot),
		fmt.Sprint("GOTMPDIR=", goproxyRoot),
	)

	cmd.Dir = goproxyRoot

	return cmd.Run()
}

// checkInfoFile checks the info file targeted by the name.
func checkInfoFile(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()

	var info struct {
		Version string
		Time    time.Time
	}

	if err := json.NewDecoder(f).Decode(&info); err != nil {
		return err
	}

	if !semver.IsValid(info.Version) || info.Time.IsZero() {
		return errors.New("invalid info file")
	}

	return nil
}

// checkModFile checks the mod file targeted by the name.
func checkModFile(name string) error {
	b, err := ioutil.ReadFile(name)
	if err != nil {
		return err
	}

	if _, err := modfile.Parse("go.mod", b, nil); err != nil {
		return fmt.Errorf("invalid mod file: %v", err)
	}

	return nil
}

// checkZIPFile checks the ZIP file targeted by the name.
func checkZIPFile(name string) error {
	rc, err := zip.OpenReader(name)
	if err != nil {
		return fmt.Errorf("invalid zip file: %v", err)
	}

	return rc.Close()
}
