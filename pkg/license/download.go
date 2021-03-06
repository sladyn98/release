/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package license

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"github.com/nozzle/throttler"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"k8s.io/release/pkg/util"
)

// ListURL is the json list of all spdx licenses
const ListURL = "https://spdx.org/licenses/licenses.json"

// NewDownloader returns a downloader with the default options
func NewDownloader() (*Downloader, error) {
	return NewDownloaderWithOptions(DefaultDownloaderOpts)
}

// NewDownloaderWithOptions returns a downloader with specific options
func NewDownloaderWithOptions(opts *DownloaderOptions) (*Downloader, error) {
	if err := opts.Validate(); err != nil {
		return nil, errors.Wrap(err, "validating downloader options")
	}
	impl := DefaultDownloaderImpl{}
	impl.SetOptions(opts)

	d := &Downloader{}
	d.SetImplementation(&impl)

	return d, nil
}

// DownloaderOptions is a set of options for the license downloader
type DownloaderOptions struct {
	EnableCache       bool   // Should we use the cache or not
	CacheDir          string // Directory where data will be cached, defaults to temporary dir
	parallelDownloads int    // Number of license downloads we'll do at once
}

// Validate Checks the downloader options
func (do *DownloaderOptions) Validate() error {
	// If we are using a cache
	if do.EnableCache {
		// Is we have a cache dir, check if it exists
		if do.CacheDir != "" {
			if !util.Exists(do.CacheDir) {
				return errors.New("the specified cache directory does not exist: " + do.CacheDir)
			}
		}
		// And no cache dir was specified
		if do.CacheDir == "" {
			dir, err := ioutil.TempDir(os.TempDir(), "license-cache-")
			if err != nil {
				return errors.Wrap(err, "creating temporary directory")
			}
			do.CacheDir = dir
		}
	}
	return nil
}

// Downloader handles downloading f license data
type Downloader struct {
	impl DownloaderImplementation
}

// SetImplementation sets the implementation that will drive the downloader
func (d *Downloader) SetImplementation(di DownloaderImplementation) {
	d.impl = di
}

// GetLicenses is the mina function of the downloader. Returns a license list
// or an error if could get them
func (d *Downloader) GetLicenses() (*SPDXLicenseList, error) {
	return d.impl.GetLicenses()
}

//counterfeiter:generate . DownloaderImplementation

// DownloaderImplementation has only one method
type DownloaderImplementation interface {
	GetLicenses() (*SPDXLicenseList, error)
	SetOptions(*DownloaderOptions)
}

// DefaultDownloaderOpts set of options for the license downloader
var DefaultDownloaderOpts = &DownloaderOptions{
	EnableCache:       true,
	CacheDir:          "",
	parallelDownloads: 5,
}

// DefaultDownloaderImpl is the default implementation that gets licenses
type DefaultDownloaderImpl struct {
	Options *DownloaderOptions
}

// SetOptions sets the implementation options
func (ddi *DefaultDownloaderImpl) SetOptions(opts *DownloaderOptions) {
	ddi.Options = opts
}

// GetLicenses downloads the main json file listing all SPDX supported licenses
func (ddi *DefaultDownloaderImpl) GetLicenses() (licenses *SPDXLicenseList, err error) {
	// TODO: Cache licenselist
	logrus.Info("Downloading main SPDX license data")

	// Get the list of licenses
	resp, err := http.Get(ListURL)
	if err != nil {
		return nil, errors.Wrap(err, "fetching licenses list")
	}
	defer resp.Body.Close()

	licensesJSON, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, errors.Wrap(err, "reading license list response body")
	}

	licenseList := &SPDXLicenseList{}
	if err := json.Unmarshal(licensesJSON, licenseList); err != nil {
		return nil, errors.Wrap(err, "parsing SPDX licence list")
	}

	logrus.Infof("Read data for %d licenses. Downloading.", len(licenseList.LicenseData))

	// Create a new Throttler that will get `parallelDownloads` urls at a time
	t := throttler.New(ddi.Options.parallelDownloads, len(licenseList.LicenseData))
	for _, l := range licenseList.LicenseData {
		// Launch a goroutine to fetch the URL.
		go func(url string) {
			var err error
			defer t.Done(err)
			l, err := ddi.getLicenseFromURL(url)
			if err != nil {
				return
			}
			licenseList.Add(l)
		}(l.DetailsURL)
		t.Throttle()
	}

	// If the throttler collected errors, return those
	if t.Err() != nil {
		return nil, t.Err()
	}
	return licenseList, nil
}

// cacheFileName return the cache filename for an URL
func (ddi *DefaultDownloaderImpl) cacheFileName(url string) string {
	return filepath.Join(
		ddi.Options.CacheDir, fmt.Sprintf("%x.json", sha1.Sum([]byte(url))),
	)
}

// cacheData writes data to a cache file
func (ddi *DefaultDownloaderImpl) cacheData(url string, data []byte) error {
	cacheFileName := ddi.cacheFileName(url)
	_, err := os.Stat(filepath.Dir(cacheFileName))
	if err != nil && os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(cacheFileName), os.FileMode(0o755)); err != nil {
			return errors.Wrap(err, "creating cache directory")
		}
	}
	return errors.Wrap(ioutil.WriteFile(cacheFileName, data, os.FileMode(0o644)), "writing cache file")
}

// getCachedData returns cached data for an URL if we have it
func (ddi *DefaultDownloaderImpl) getCachedData(url string) ([]byte, error) {
	cacheFileName := ddi.cacheFileName(url)
	finfo, err := os.Stat(cacheFileName)
	if err != nil && !os.IsNotExist(err) {
		return nil, errors.Wrap(err, "checking if cached data exists")
	}

	if err != nil {
		logrus.Debugf("No cached data for %s", url)
		return nil, nil
	}

	if finfo.Size() == 0 {
		logrus.Warn("Cached file is empty, removing")
		return nil, errors.Wrap(os.Remove(cacheFileName), "removing corrupt cached file")
	}
	licensesJSON, err := ioutil.ReadFile(cacheFileName)
	if err != nil {
		return nil, errors.Wrap(err, "reading cached data file")
	}
	return licensesJSON, nil
}

// getLicenseFromURL downloads a license in json and returns it parsed into a struct
func (ddi *DefaultDownloaderImpl) getLicenseFromURL(url string) (license *SPDXLicense, err error) {
	licenseJSON := []byte{}
	// Determine the cache file name
	if ddi.Options.EnableCache {
		licenseJSON, err = ddi.getCachedData(url)
		if err != nil {
			return nil, errors.Wrap(err, "checking download cache")
		}
		if len(licenseJSON) > 0 {
			logrus.Debugf("Data for %s is already cached", url)
		}
	}

	// If we still don't have json data, download it
	if len(licenseJSON) == 0 {
		logrus.Infof("Downloading license data from %s", url)
		resp, err := http.Get(url)
		if err != nil {
			return nil, errors.Wrapf(err, "getting %s", url)
		}
		defer resp.Body.Close()
		licenseJSON, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, errors.Wrap(err, "reading response body")
		}

		logrus.Infof("Downloaded %d bytes from %s", len(licenseJSON), url)

		if ddi.Options.EnableCache {
			if err := ddi.cacheData(url, licenseJSON); err != nil {
				return nil, errors.Wrap(err, "caching url data")
			}
		}
	}

	return ParseSPDXLicense(licenseJSON)
}
