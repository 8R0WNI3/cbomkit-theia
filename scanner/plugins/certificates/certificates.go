package certificates

import (
	"encoding/pem"
	"errors"
	"ibm/container-image-cryptography-scanner/provider/filesystem"
	scanner_errors "ibm/container-image-cryptography-scanner/scanner/errors"
	"ibm/container-image-cryptography-scanner/scanner/plugins"
	"log/slog"
	"path/filepath"
	"slices"

	"go.mozilla.org/pkcs7"
	"golang.org/x/exp/rand"

	bomdag "ibm/container-image-cryptography-scanner/scanner/bom-dag"

	cdx "github.com/CycloneDX/cyclonedx-go"
	"github.com/google/uuid"
)

// Plugin to parse certificates from the filesystem
type CertificatesPlugin struct{}

// Get the name of the plugin
func (certificatesPlugin *CertificatesPlugin) GetName() string {
	return "Certificate File Plugin"
}

// Get the type of the plugin
func (certificatesPlugin *CertificatesPlugin) GetType() plugins.PluginType {
	return plugins.PluginTypeAppend
}

// Parse all certificates from the given filesystem
func NewCertificatePlugin() (plugins.Plugin, error) {
	return &CertificatesPlugin{}, nil
}

// Add the found certificates to the slice of components
func (certificatesPlugin *CertificatesPlugin) UpdateBOM(fs filesystem.Filesystem, bom *cdx.BOM) error {
	certificates := []*x509CertificateWithMetadata{}

	err := fs.WalkDir(
		func(path string) (err error) {
			switch filepath.Ext(path) {
			case ".pem", ".cer", ".cert", ".der", ".ca-bundle", ".crt":
				raw, err := fs.ReadFile(path)
				if err != nil {
					return err
				}
				certs, err := certificatesPlugin.parsex509CertFromPath(raw, path)
				if err != nil {
					return scanner_errors.GetParsingFailedAlthoughCheckedError(err, certificatesPlugin.GetName())
				}
				certificates = append(certificates, certs...)
			case ".p7a", ".p7b", ".p7c", ".p7r", ".p7s", ".spc":
				raw, err := fs.ReadFile(path)
				if err != nil {
					return err
				}
				certs, err := certificatesPlugin.parsePKCS7FromPath(raw, path)
				if err != nil {
					return scanner_errors.GetParsingFailedAlthoughCheckedError(err, certificatesPlugin.GetName())
				}
				certificates = append(certificates, certs...)
			default:
				return err
			}

			return err
		})

	if err != nil {
		return err
	}

	slog.Info("Certificate searching done", "count", len(certificates))

	// This ensures that the generated UUIDs are deterministic
	uuid.SetRand(rand.New(rand.NewSource(1)))

	dag := bomdag.NewBomDAG()

	for _, cert := range certificates {
		certDAG, err := cert.generateDAG()
		if errors.Is(err, errX509UnknownAlgorithm) {
			slog.Info("X.509 certs contained unknown algorithms. Continuing anyway", "errors", err)
		} else if err != nil {
			return err
		}

		if err := dag.Merge(certDAG); err != nil {
			slog.Error("Merging of DAGs failed", "certificate path", cert.path)
			return err
		}
	}

	components, dependencyMap, err := dag.GetCDXComponents()

	if err != nil {
		return err
	}

	//file, _ := os.Create("./mygraph.gv")
	//_ = draw.DOT(dag, file)

	if bom.Components == nil {
		comps := make([]cdx.Component, 0, len(components))
		bom.Components = &comps
	}
	*bom.Components = append(*bom.Components, components...)
	if bom.Dependencies == nil {
		deps := make([]cdx.Dependency, 0, len(dependencyMap))
		bom.Dependencies = &deps
	}
	*bom.Dependencies = MergeDependencyStructSlice(*bom.Dependencies, dependencyMapToStructSlice(dependencyMap))

	return nil
}

// Parse a X.509 certificate from the given path (in base64 PEM or binary DER)
func (certificatesPlugin *CertificatesPlugin) parsex509CertFromPath(raw []byte, path string) ([]*x509CertificateWithMetadata, error) {
	rest := raw
	var blocks []*pem.Block
	for len(rest) != 0 {
		var newBlock *pem.Block
		newBlock, rest = pem.Decode(rest)
		if newBlock != nil {
			if newBlock.Type != "CERTIFICATE" {
				slog.Warn("PEM file contains part that is not yet supported, continuing anyway", "unsupported_type", newBlock.Type)
				continue
			}
			blocks = append(blocks, newBlock)
		} else {
			break
		}
	}

	if len(blocks) == 0 {
		return parseCertificatesToX509CertificateWithMetadata(raw, path)
	}

	certs := make([]*x509CertificateWithMetadata, 0, len(blocks))

	for _, block := range blocks {
		moreCerts, err := parseCertificatesToX509CertificateWithMetadata(block.Bytes, path)
		if err != nil {
			return moreCerts, err
		}
		certs = append(certs, moreCerts...)
	}

	return certs, nil
}

// Parse X.509 certificates from a PKCS7 file (base64 PEM format)
func (certificatesPlugin CertificatesPlugin) parsePKCS7FromPath(raw []byte, path string) ([]*x509CertificateWithMetadata, error) {
	block, _ := pem.Decode(raw)

	pkcs7Object, err := pkcs7.Parse(block.Bytes)
	if err != nil || pkcs7Object == nil {
		return make([]*x509CertificateWithMetadata, 0), err
	}

	certsWithMetadata := make([]*x509CertificateWithMetadata, 0, len(pkcs7Object.Certificates))

	for _, cert := range pkcs7Object.Certificates {
		certWithMetadata, err := newX509CertificateWithMetadata(cert, path)
		if err != nil {
			return make([]*x509CertificateWithMetadata, 0), err
		}
		certsWithMetadata = append(certsWithMetadata, certWithMetadata)
	}

	return certsWithMetadata, nil
}

// Check if comp is contained in list while ignoring BOMReferences
func strippedAlgorithmContains(comp cdx.Component, list []cdx.Component) (bool, cdx.Component) {
	if comp.CryptoProperties.AssetType != cdx.CryptoAssetTypeAlgorithm {
		panic("scanner: strippedAlgorithmContains was called on a non-algorithm component")
	}
	for _, comp2 := range list {
		if comp2.CryptoProperties.AssetType == cdx.CryptoAssetTypeAlgorithm && strippedAlgorithmEquals(comp, comp2) {
			return true, comp2
		}
	}

	return false, cdx.Component{}
}

// Check if a equals b while ignoring BOMReferences
func strippedAlgorithmEquals(a cdx.Component, b cdx.Component) bool {
	if a.CryptoProperties.AssetType != cdx.CryptoAssetTypeAlgorithm || b.CryptoProperties.AssetType != cdx.CryptoAssetTypeAlgorithm {
		panic("scanner: strippedAlgorithmEquals was called on a non-algorithm component")
	}

	return a.Name == b.Name &&
		a.CryptoProperties.AlgorithmProperties.Primitive == b.CryptoProperties.AlgorithmProperties.Primitive &&
		a.CryptoProperties.AlgorithmProperties.ExecutionEnvironment == b.CryptoProperties.AlgorithmProperties.ExecutionEnvironment &&
		a.CryptoProperties.AlgorithmProperties.ImplementationPlatform == b.CryptoProperties.AlgorithmProperties.ImplementationPlatform &&
		slices.Equal(*a.CryptoProperties.AlgorithmProperties.CertificationLevel, *b.CryptoProperties.AlgorithmProperties.CertificationLevel) &&
		slices.Equal(*a.CryptoProperties.AlgorithmProperties.CryptoFunctions, *b.CryptoProperties.AlgorithmProperties.CryptoFunctions) &&
		a.CryptoProperties.OID == b.CryptoProperties.OID &&
		a.CryptoProperties.AlgorithmProperties.Padding == b.CryptoProperties.AlgorithmProperties.Padding
}

// Replace all usages of oldRef by newRef in components
func replaceBomRefUsages(oldRef cdx.BOMReference, newRef cdx.BOMReference, components *[]cdx.Component) {
	for _, comp := range *components {
		if comp.BOMRef == string(oldRef) {
			comp.BOMRef = string(newRef)
		}

		if comp.CryptoProperties != nil {
			if comp.CryptoProperties.CertificateProperties != nil {
				if comp.CryptoProperties.CertificateProperties.SignatureAlgorithmRef == oldRef {
					comp.CryptoProperties.CertificateProperties.SignatureAlgorithmRef = newRef
					continue
				}
				if comp.CryptoProperties.CertificateProperties.SubjectPublicKeyRef == oldRef {
					comp.CryptoProperties.CertificateProperties.SubjectPublicKeyRef = newRef
					continue
				}
			} else if comp.CryptoProperties.RelatedCryptoMaterialProperties != nil {
				if comp.CryptoProperties.RelatedCryptoMaterialProperties.AlgorithmRef == oldRef {
					comp.CryptoProperties.RelatedCryptoMaterialProperties.AlgorithmRef = newRef
					continue
				}
			}
		}
	}
}

func dependencyMapToStructSlice(dependencyMap map[cdx.BOMReference][]string) []cdx.Dependency {
	dependencies := make([]cdx.Dependency, 0)

	for ref, dependsOn := range dependencyMap {
		dependencies = append(dependencies, cdx.Dependency{
			Ref:          string(ref),
			Dependencies: &dependsOn,
		})
	}

	return dependencies
}

func MergeDependencyStructSlice(this []cdx.Dependency, other []cdx.Dependency) []cdx.Dependency {
	for _, otherStruct := range other {
		i := IndexBomRefInDependencySlice(this, cdx.BOMReference(otherStruct.Ref))
		if i != -1 {
			// Merge
			for _, s := range *otherStruct.Dependencies {
				if !slices.Contains(*this[i].Dependencies, s) {
					*this[i].Dependencies = append(*this[i].Dependencies, s)
				}
			}
		} else {
			this = append(this, otherStruct)
		}
	}
	return this
}

// Return index in slice if bomRef is found in slice or -1 if not present
func IndexBomRefInDependencySlice(slice []cdx.Dependency, bomRef cdx.BOMReference) int {
	for i, dep := range slice {
		if dep.Ref == string(bomRef) {
			return i
		}
	}

	return -1
}
