package regclient

import (
	"context"
	// crypto libraries included for go-digest
	_ "crypto/sha256"
	_ "crypto/sha512"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	dockercfg "github.com/docker/cli/cli/config"
	"github.com/docker/distribution/reference"
	"github.com/docker/docker/pkg/archive"
	digest "github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

type tlsConf int

var (
	// MediaTypeDocker2Manifest is the media type when pulling manifests from a v2 registry
	MediaTypeDocker2Manifest     = "application/vnd.docker.distribution.manifest.v2+json"
	MediaTypeDocker2ManifestList = "application/vnd.docker.distribution.manifest.list.v2+json"
	// MediaTypeDocker2ImageConfig is for the configuration json object
	MediaTypeDocker2ImageConfig = "application/vnd.docker.container.image.v1+json"
)

const (
	tlsEnabled tlsConf = iota
	tlsInsecure
	tlsDisabled
)

// RegClient provides an interfaces to working with registries
type RegClient interface {
	Auth() AuthClient
	BlobGet(ctx context.Context, ref Ref, digest string, accepts []string) (io.ReadCloser, *http.Response, error)
	ImageExport(ctx context.Context, ref Ref, outStream io.Writer) error
	ImageInspect(ctx context.Context, ref Ref) (ociv1.Image, error)
	ManifestGet(ctx context.Context, ref Ref) (ociv1.Manifest, error)
	ManifestListGet(ctx context.Context, ref Ref) (ociv1.Index, error)
	TagsList(ctx context.Context, ref Ref) (TagList, error)
}

// TagList comes from github.com/opencontainers/distribution-spec,
// switch to their implementation when it becomes stable
type TagList struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

// Ref reference to a registry/repository
// If the tag or digest is available, it's also included in the reference.
// Reference itself is the unparsed string.
// While this is currently a struct, that may change in the future and access
// to contents should not be assumed/used.
type Ref struct {
	Reference, Registry, Repository, Tag, Digest string
}

type regClient struct {
	hosts      map[string]*regHost
	auth       AuthClient
	retryLimit int
}

type regHost struct {
	scheme    string
	tls       tlsConf
	dnsNames  []string
	transport *http.Transport
}

// used by image import/export to match docker expected format
type imageManifest struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// Opt functions are used to configure NewRegClient
type Opt func(*regClient)

// NewRegClient returns a registry client
func NewRegClient(opts ...Opt) RegClient {
	var rc regClient

	// TODO: move hardcoded host references into vars defined in another file
	rc.hosts = map[string]*regHost{"docker.io": {scheme: "https", tls: tlsEnabled, dnsNames: []string{"registry-1.docker.io"}}}
	rc.auth = NewAuthClient()
	rc.retryLimit = 3

	for _, opt := range opts {
		opt(&rc)
	}

	return &rc
}

// WithDockerCerts adds certificates trusted by docker in /etc/docker/certs.d
func WithDockerCerts() Opt {
	return func(rc *regClient) {
		return
	}
}

// WithDockerCreds adds configuration from users docker config with registry logins
func WithDockerCreds() Opt {
	return func(rc *regClient) {
		conffile := dockercfg.LoadDefaultConfigFile(os.Stderr)
		creds, err := conffile.GetAllCredentials()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to load docker creds %s\n", err)
			return
		}
		for _, cred := range creds {
			// fmt.Printf("Processing cred %v\n", cred)
			// TODO: clean this up, get index and registry-1 from variables
			if cred.ServerAddress == "https://index.docker.io/v1/" && cred.Username != "" && cred.Password != "" {
				rc.auth.Set("registry-1.docker.io", cred.Username, cred.Password)
			} else if cred.ServerAddress != "" && cred.Username != "" && cred.Password != "" {
				rc.auth.Set(cred.ServerAddress, cred.Username, cred.Password)
			}
		}
		return
	}
}

// WithRegClientConf adds configuration from regcli configuration file (yml?)
func WithRegClientConf() Opt {
	return func(rc *regClient) {
		return
	}
}

// NewRef returns a repository reference including a registry, repository (path), digest, and tag
func NewRef(ref string) (Ref, error) {
	parsed, err := reference.ParseNormalizedNamed(ref)

	var ret Ref
	ret.Reference = ref

	if err != nil {
		return ret, err
	}

	ret.Registry = reference.Domain(parsed)
	ret.Repository = reference.Path(parsed)

	if canonical, ok := parsed.(reference.Canonical); ok {
		ret.Digest = canonical.Digest().String()
	}

	if tagged, ok := parsed.(reference.Tagged); ok {
		ret.Tag = tagged.Tag()
	}

	return ret, nil
}

// CommonName outputs a parsable name from a reference
func (r Ref) CommonName() string {
	cn := ""
	if r.Registry != "" {
		cn = r.Registry + "/"
	}
	if r.Repository == "" {
		return ""
	}
	cn = cn + r.Repository
	if r.Tag != "" {
		cn = cn + ":" + r.Tag
	}
	if r.Digest != "" {
		cn = cn + "@" + r.Digest
	}
	return cn
}

func (rc *regClient) Auth() AuthClient {
	return rc.auth
}

func (rc *regClient) BlobGet(ctx context.Context, ref Ref, digest string, accepts []string) (io.ReadCloser, *http.Response, error) {
	host := rc.getHost(ref.Registry)

	blobURL := url.URL{
		Scheme: host.scheme,
		Host:   host.dnsNames[0],
		Path:   "/v2/" + ref.Repository + "/blobs/" + digest,
	}

	req, err := http.NewRequest("GET", blobURL.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	for _, accept := range accepts {
		req.Header.Add("Accept", accept)
	}

	rty := rc.newRetryableForHost(host)
	resp, err := rty.Req(ctx, rc, req)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp, nil
}

func (rc *regClient) ImageExport(ctx context.Context, ref Ref, outStream io.Writer) error {
	if ref.CommonName() == "" {
		return ErrNotFound
	}

	expManifest := imageManifest{}
	expManifest.RepoTags = append(expManifest.RepoTags, ref.CommonName())

	m, err := rc.ManifestGet(ctx, ref)
	if err != nil {
		return err
	}

	// write to a temp directory
	tempDir, err := ioutil.TempDir("", "regcli-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tempDir)

	fmt.Fprintf(os.Stderr, "Debug: Using temp directory for export \"%s\"\n", tempDir)

	// retrieve the config blob
	confio, _, err := rc.BlobGet(ctx, ref, m.Config.Digest.String(), []string{MediaTypeDocker2ImageConfig, ociv1.MediaTypeImageConfig})
	if err != nil {
		return err
	}
	confstr, err := ioutil.ReadAll(confio)
	if err != nil {
		return err
	}
	confDigest := digest.FromBytes(confstr)
	if m.Config.Digest != confDigest {
		fmt.Fprintf(os.Stderr, "Warning: digest for image config does not match, pulled %s, calculated %s\n", m.Config.Digest.String(), confDigest.String())
	}
	conf := ociv1.Image{}
	err = json.Unmarshal(confstr, &conf)
	if err != nil {
		return err
	}
	// reset the rootfs DiffIDs and recalculate them as layers are downloaded from the manifest
	// layer digest will change when decompressed and docker load expects layers as tar files
	conf.RootFS.DiffIDs = []digest.Digest{}

	for _, layerDesc := range m.Layers {
		// TODO: wrap layer download in a concurrency throttled goroutine
		// create tempdir for layer
		layerDir, err := ioutil.TempDir(tempDir, "layer-*")
		if err != nil {
			return err
		}
		// no need to defer remove of layerDir, it is inside of tempDir

		// request layer
		layerRComp, _, err := rc.BlobGet(ctx, ref, layerDesc.Digest.String(), []string{})
		if err != nil {
			return err
		}
		// handle any failures before reading to a file
		defer layerRComp.Close()
		// gather digest of compressed stream to verify downloaded blob
		digestComp := digest.Canonical.Digester()
		trComp := io.TeeReader(layerRComp, digestComp.Hash())
		// decompress layer
		layerTarStream, err := archive.DecompressStream(trComp)
		if err != nil {
			return err
		}
		// generate digest of decompressed layer
		digestTar := digest.Canonical.Digester()
		tr := io.TeeReader(layerTarStream, digestTar.Hash())

		// download to a temp location
		layerTarFile := filepath.Join(layerDir, "layer.tar")
		lf, err := os.OpenFile(layerTarFile, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			return err
		}
		_, err = io.Copy(lf, tr)
		if err != nil {
			return err
		}
		lf.Close()

		// verify digests
		if layerDesc.Digest != digestComp.Digest() {
			fmt.Fprintf(os.Stderr, "Warning: digest for layer does not match, pulled %s, calculated %s\n", layerDesc.Digest.String(), digestComp.Digest().String())
		}

		// update references to uncompressed tar digest in the filesystem, manifest, and image config
		digestFull := digestTar.Digest()
		digestHex := digestFull.Encoded()
		digestDir := filepath.Join(tempDir, digestHex)
		digestFile := filepath.Join(digestHex, "layer.tar")
		digestFileFull := filepath.Join(tempDir, digestFile)
		if err := os.Rename(layerDir, digestDir); err != nil {
			return err
		}
		if err := os.Chtimes(digestFileFull, *conf.Created, *conf.Created); err != nil {
			return err
		}
		expManifest.Layers = append(expManifest.Layers, digestFile)
		conf.RootFS.DiffIDs = append(conf.RootFS.DiffIDs, digestFull)
	}
	// TODO: if using goroutines, wait for all layers to finish

	// calc config digest and write to file
	confstr, err = json.Marshal(conf)
	if err != nil {
		return err
	}
	confDigest = digest.Canonical.FromBytes(confstr)
	confFile := confDigest.Encoded() + ".json"
	confFileFull := filepath.Join(tempDir, confFile)
	if err := ioutil.WriteFile(confFileFull, confstr, 0644); err != nil {
		return err
	}
	if err := os.Chtimes(confFileFull, *conf.Created, *conf.Created); err != nil {
		return err
	}
	expManifest.Config = confFile

	// write manifest
	ml := []imageManifest{expManifest}
	mlj, err := json.Marshal(ml)
	if err != nil {
		return err
	}
	manifestFile := filepath.Join(tempDir, "manifest.json")
	if err := ioutil.WriteFile(manifestFile, mlj, 0644); err != nil {
		return err
	}
	if err := os.Chtimes(manifestFile, time.Unix(0, 0), time.Unix(0, 0)); err != nil {
		return err
	}

	// package in tar file
	fs, err := archive.Tar(tempDir, archive.Uncompressed)
	if err != nil {
		return err
	}
	defer fs.Close()

	_, err = io.Copy(outStream, fs)

	return nil
}

func (rc *regClient) ImageInspect(ctx context.Context, ref Ref) (ociv1.Image, error) {
	img := ociv1.Image{}

	m, err := rc.ManifestGet(ctx, ref)
	if err != nil {
		return img, err
	}

	imgIO, _, err := rc.BlobGet(ctx, ref, m.Config.Digest.String(), []string{MediaTypeDocker2ImageConfig, ociv1.MediaTypeImageConfig})
	if err != nil {
		return img, err
	}

	imgBody, err := ioutil.ReadAll(imgIO)
	if err != nil {
		return img, err
	}
	// fmt.Printf("Body:\n%s\n", respBody)
	err = json.Unmarshal(imgBody, &img)
	if err != nil {
		return img, err
	}
	return img, nil
}

func (rc *regClient) ManifestGet(ctx context.Context, ref Ref) (ociv1.Manifest, error) {
	m := ociv1.Manifest{}

	host := rc.getHost(ref.Registry)
	var tagOrDigest string
	if ref.Digest != "" {
		tagOrDigest = ref.Digest
	} else if ref.Tag != "" {
		tagOrDigest = ref.Tag
	} else {
		return m, ErrMissingTag
	}

	manfURL := url.URL{
		Scheme: host.scheme,
		Host:   host.dnsNames[0],
		Path:   "/v2/" + ref.Repository + "/manifests/" + tagOrDigest,
	}

	req, err := http.NewRequest("GET", manfURL.String(), nil)
	if err != nil {
		return m, err
	}
	req.Header.Add("Accept", MediaTypeDocker2Manifest)
	req.Header.Add("Accept", ociv1.MediaTypeImageManifest)

	rty := rc.newRetryableForHost(host)
	resp, err := rty.Req(ctx, rc, req)
	if err != nil {
		return m, err
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(respBody, &m)
	if err != nil {
		return m, err
	}

	return m, nil
}

func (rc *regClient) ManifestListGet(ctx context.Context, ref Ref) (ociv1.Index, error) {
	ml := ociv1.Index{}

	host := rc.getHost(ref.Registry)
	var tagOrDigest string
	if ref.Digest != "" {
		tagOrDigest = ref.Digest
	} else if ref.Tag != "" {
		tagOrDigest = ref.Tag
	} else {
		return ml, ErrMissingTag
	}

	manfURL := url.URL{
		Scheme: host.scheme,
		Host:   host.dnsNames[0],
		Path:   "/v2/" + ref.Repository + "/manifests/" + tagOrDigest,
	}

	req, err := http.NewRequest("GET", manfURL.String(), nil)
	if err != nil {
		return ml, err
	}
	req.Header.Add("Accept", MediaTypeDocker2ManifestList)
	req.Header.Add("Accept", ociv1.MediaTypeImageIndex)

	rty := rc.newRetryableForHost(host)
	resp, err := rty.Req(ctx, rc, req)
	if err != nil {
		return ml, err
	}

	// docker will respond for a manifestlist request with a manifest, so check the content type
	ct := resp.Header.Get("Content-Type")
	if ct != MediaTypeDocker2ManifestList && ct != ociv1.MediaTypeImageIndex {
		return ml, ErrNotFound
	}

	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return ml, err
	}

	err = json.Unmarshal(respBody, &ml)
	if err != nil {
		return ml, err
	}

	return ml, nil
}

func (rc *regClient) TagsList(ctx context.Context, ref Ref) (TagList, error) {
	tl := TagList{}
	host := rc.getHost(ref.Registry)
	repoURL := url.URL{
		Scheme: host.scheme,
		Host:   host.dnsNames[0],
		Path:   "/v2/" + ref.Repository + "/tags/list",
	}

	req, err := http.NewRequest("GET", repoURL.String(), nil)
	if err != nil {
		return tl, err
	}
	rty := rc.newRetryableForHost(host)
	resp, err := rty.Req(ctx, rc, req)
	if err != nil {
		return tl, err
	}
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return tl, err
	}
	err = json.Unmarshal(respBody, &tl)
	if err != nil {
		return tl, err
	}

	return tl, nil
}

func (rc *regClient) getHost(hostname string) *regHost {
	host, ok := rc.hosts[hostname]
	if !ok {
		host = &regHost{scheme: "https", tls: tlsEnabled, dnsNames: []string{hostname}}
		rc.hosts[hostname] = host
	}
	return host
}

func (rc *regClient) newRetryableForHost(host *regHost) Retryable {
	if host.transport == nil {
		tlsc := &tls.Config{}
		if host.tls == tlsInsecure {
			tlsc.InsecureSkipVerify = true
		}
		// TODO: update tlsc based on host config for host specific certs and client key/cert pair
		t := &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:       30 * time.Second,
				KeepAlive:     30 * time.Second,
				FallbackDelay: 300 * time.Millisecond,
			}).DialContext,
			MaxIdleConns:          10,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			TLSClientConfig:       tlsc,
			ExpectContinueTimeout: 5 * time.Second,
		}
		host.transport = t
	}
	r := NewRetryable(RetryWithTransport(host.transport), RetryWithLimit(rc.retryLimit))
	return r
}
