package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/apis/acme/v1alpha1"
	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	"github.com/go-gandi/go-gandi"
	"github.com/go-gandi/go-gandi/config"
	extapi "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"os"
	"strings"
)

const (
	GandiMinTtl = 300 // Gandi reports an error for values < this value
)

var GroupName = os.Getenv("GROUP_NAME")

func main() {
	klog.InitFlags(nil)
	if GroupName == "" {
		panic("GROUP_NAME must be specified")
	}

	// This will register our gandi DNS provider with the webhook serving
	// library, making it available as an API under the provided GroupName.
	// You can register multiple DNS provider implementations with a single
	// webhook, where the Name() method will be used to disambiguate between
	// the different implementations.
	cmd.RunWebhookServer(GroupName,
		&gandiDNSProviderSolver{},
	)
}

// gandiDNSProviderSolver implements the provider-specific logic needed to
// 'present' an ACME challenge TXT record for your own DNS provider.
// To do so, it must implement the `github.com/cert-manager/cert-manager/pkg/acme/webhook.Solver`
// interface.
type gandiDNSProviderSolver struct {
	client *kubernetes.Clientset
}

// gandiDNSProviderConfig is a structure that is used to decode into when
// solving a DNS01 challenge.
// This information is provided by cert-manager, and may be a reference to
// additional configuration that's needed to solve the challenge for this
// particular certificate or issuer.
// This typically includes references to Secret resources containing DNS
// provider credentials, in cases where a 'multi-tenant' DNS solver is being
// created.
// If you do *not* require per-issuer or per-certificate configuration to be
// provided to your webhook, you can skip decoding altogether in favour of
// using CLI flags or similar to provide configuration.
// You should not include sensitive information here. If credentials need to
// be used by your provider here, you should reference a Kubernetes Secret
// resource and fetch these credentials using a Kubernetes clientset.
type gandiDNSProviderConfig struct {
	// These fields will be set by users in the
	// `issuer.spec.acme.dns01.providers.webhook.config` field.
	APIKeySecretRef cmmeta.SecretKeySelector `json:"apiKeySecretRef"`
}

// Name is used as the name for this DNS solver when referencing it on the ACME
// Issuer resource.
// This should be unique **within the group name**, i.e. you can have two
// solvers configured with the same Name() **so long as they do not co-exist
// within a single webhook deployment**.
// For example, `cloudflare` may be used as the name of a solver.
func (c *gandiDNSProviderSolver) Name() string {
	return "gandi"
}

func extractRootAndSubDomain(fqdn string, entry string) (string, string, error) {
	parts := strings.Split(strings.Trim(fqdn, "."), ".")
	domain := parts[len(parts)-2] + "." + parts[len(parts)-1]

	return domain, strings.Join(append([]string{strings.Trim(entry, ".")}, parts[0:len(parts)-2]...), "."), nil
}

// Present is responsible for actually presenting the DNS record with the
// DNS provider.
// This method should tolerate being called multiple times with the same value.
// cert-manager itself will later perform a self check to ensure that the
// solver has correctly configured the DNS provider.
func (c *gandiDNSProviderSolver) Present(ch *v1alpha1.ChallengeRequest) error {
	klog.V(6).Infof("call function Present: namespace=%s, zone=%s, fqdn=%s",
		ch.ResourceNamespace, ch.ResolvedZone, ch.ResolvedFQDN)

	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return fmt.Errorf("unable to load config: %v", err)
	}

	klog.V(6).Infof("decoded configuration %v", cfg)

	apiKey, err := c.getApiKey(&cfg, ch.ResourceNamespace)
	if err != nil {
		return fmt.Errorf("unable to get API key: %v", err)
	}

	clientcfg := &config.Config{
		APIKey: *apiKey,
		Debug:  false,
		DryRun: false,
	}
	gandiClient := gandi.NewLiveDNSClient(*clientcfg)

	entry, domain := c.getDomainAndEntry(ch)
	klog.V(6).Infof("present for entry=%s, domain=%s", entry, domain)

	root, subdomain, err := extractRootAndSubDomain(domain, entry)
	if err != nil {
		return fmt.Errorf("unable to mange provided domain : %v", err)
	}

	record, err := gandiClient.GetDomainRecordByNameAndType(root, subdomain, "TXT")
	if err != nil {
		klog.V(6).Infof("There is no entry of TXT matching, creating a new one for %s with value \"%s\"", subdomain+root, ch.Key)
		_, err := gandiClient.CreateDomainRecord(root, subdomain, "TXT", GandiMinTtl, []string{ch.Key})
		if err != nil {
			return fmt.Errorf("unable to create TXT record: %v", err)
		}
	} else {
		if strings.Join(record.RrsetValues, "") != "\""+ch.Key+"\"" {
			klog.V(6).Infof("Current record exists for %s value is %s, new value will be \"%s\"", subdomain+root, strings.Join(record.RrsetValues, ""), ch.Key)
			_, err := gandiClient.UpdateDomainRecordByNameAndType(root, subdomain, "TXT", GandiMinTtl, []string{ch.Key})
			if err != nil {
				return fmt.Errorf("unable to update TXT record: %v", err)
			}
		}
	}
	return nil
}

// CleanUp should delete the relevant TXT record from the DNS provider console.
// If multiple TXT records exist with the same record name (e.g.
// _acme-challenge.example.com) then **only** the record with the same `key`
// value provided on the ChallengeRequest should be cleaned up.
// This is in order to facilitate multiple DNS validations for the same domain
// concurrently.
func (c *gandiDNSProviderSolver) CleanUp(ch *v1alpha1.ChallengeRequest) error {
	klog.V(6).Infof("call function CleanUp: namespace=%s, zone=%s, fqdn=%s",
		ch.ResourceNamespace, ch.ResolvedZone, ch.ResolvedFQDN)

	cfg, err := loadConfig(ch.Config)
	if err != nil {
		return fmt.Errorf("unable to load config: %v", err)
	}

	klog.V(6).Infof("decoded configuration %v", cfg)

	apiKey, err := c.getApiKey(&cfg, ch.ResourceNamespace)
	if err != nil {
		return fmt.Errorf("unable to get API key: %v", err)
	}

	clientcfg := &config.Config{
		APIKey: *apiKey,
		Debug:  true,
		DryRun: false,
	}
	gandiClient := gandi.NewLiveDNSClient(*clientcfg)

	entry, domain := c.getDomainAndEntry(ch)

	root, subdomain, err := extractRootAndSubDomain(domain, entry)
	if err != nil {
		return fmt.Errorf("unable to mange provided domain : %v", err)
	}

	_, err = gandiClient.GetDomainRecordByNameAndType(root, subdomain, "TXT")
	if err != nil {
		klog.V(6).Infof("There is no entry of TXT matching, do nothing", subdomain+root, ch.Key)
	} else {
		err := gandiClient.DeleteDomainRecord(root, subdomain, "TXT")
		if err != nil {
			return fmt.Errorf("unable to delete TXT record: %v", err)
		}
	}

	return nil
}

// Initialize will be called when the webhook first starts.
// This method can be used to instantiate the webhook, i.e. initialising
// connections or warming up caches.
// Typically, the kubeClientConfig parameter is used to build a Kubernetes
// client that can be used to fetch resources from the Kubernetes API, e.g.
// Secret resources containing credentials used to authenticate with DNS
// provider accounts.
// The stopCh can be used to handle early termination of the webhook, in cases
// where a SIGTERM or similar signal is sent to the webhook process.
func (c *gandiDNSProviderSolver) Initialize(kubeClientConfig *rest.Config, _ <-chan struct{}) error {
	klog.V(6).Infof("call function Initialize")
	cl, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return fmt.Errorf("unable to get k8s client: %v", err)
	}
	c.client = cl
	return nil
}

// loadConfig is a small helper function that decodes JSON configuration into
// the typed config struct.
func loadConfig(cfgJSON *extapi.JSON) (gandiDNSProviderConfig, error) {
	cfg := gandiDNSProviderConfig{}
	// handle the 'base case' where no configuration has been provided
	if cfgJSON == nil {
		return cfg, nil
	}
	if err := json.Unmarshal(cfgJSON.Raw, &cfg); err != nil {
		return cfg, fmt.Errorf("error decoding solver config: %v", err)
	}

	return cfg, nil
}

func (c *gandiDNSProviderSolver) getDomainAndEntry(ch *v1alpha1.ChallengeRequest) (string, string) {
	// Both ch.ResolvedZone and ch.ResolvedFQDN end with a dot: '.'
	entry := strings.TrimSuffix(ch.ResolvedFQDN, ch.ResolvedZone)
	entry = strings.TrimSuffix(entry, ".")
	domain := strings.TrimSuffix(ch.ResolvedZone, ".")
	return entry, domain
}

// Get Gandi API key from Kubernetes secret.
func (c *gandiDNSProviderSolver) getApiKey(cfg *gandiDNSProviderConfig, namespace string) (*string, error) {
	secretName := cfg.APIKeySecretRef.LocalObjectReference.Name

	klog.V(6).Infof("try to load secret `%s` with key `%s`", secretName, cfg.APIKeySecretRef.Key)

	sec, err := c.client.CoreV1().Secrets(namespace).Get(context.Background(), secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("unable to get secret `%s`; %v", secretName, err)
	}

	secBytes, ok := sec.Data[cfg.APIKeySecretRef.Key]
	if !ok {
		return nil, fmt.Errorf("key %q not found in secret \"%s/%s\"", cfg.APIKeySecretRef.Key,
			cfg.APIKeySecretRef.LocalObjectReference.Name, namespace)
	}

	apiKey := string(secBytes)
	return &apiKey, nil
}
