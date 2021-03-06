package inwx

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/StackExchange/dnscontrol/v3/models"
	"github.com/StackExchange/dnscontrol/v3/pkg/diff"
	"github.com/StackExchange/dnscontrol/v3/providers"

	"github.com/nrdcg/goinwx"
	"github.com/pquerna/otp/totp"
)

/*
INWX Registrar and DNS provider

Info required in `creds.json`:
	- username
	- password

Either of the following settings is required when two factor authentication is enabled:
	- totp (TOTP code if 2FA is enabled; best specified as an env variable)
	- totp-key (shared TOTP secret used to generate a valid TOTP code; not recommended since
	            this effectively defeats the purpose of two factor authentication by storing
	            both factors at the same place)

Additional settings available in `creds.json`:
	- sandbox (set to 1 to use the sandbox API from INWX)

*/

// InwxApi is a thin wrapper around goinwx.Client.
type InwxApi struct {
	client  *goinwx.Client
	sandbox bool
}

// InwxDefaultNs contains the default INWX nameservers.
var InwxDefaultNs = []string{"ns.inwx.de", "ns2.inwx.de", "ns3.inwx.eu", "ns4.inwx.com", "ns5.inwx.net"}

// InwxSandboxDefaultNs contains the default INWX nameservers in the sandbox / OTE.
var InwxSandboxDefaultNs = []string{"ns.ote.inwx.de", "ns2.ote.inwx.de"}

// features is used to let dnscontrol know which features are supported by INWX.
var features = providers.DocumentationNotes{
	providers.CanUseAlias:            providers.Cannot("INWX does not support the ALIAS or ANAME record type."),
	providers.CanUseCAA:              providers.Can(),
	providers.CanUseDS:               providers.Unimplemented("DS records are only supported at the apex and require a different API call that hasn't been implemented yet."),
	providers.CanUsePTR:              providers.Can("PTR records with empty targets are not supported"),
	providers.CanUseNAPTR:            providers.Can(),
	providers.CanUseSRV:              providers.Can("SRV records with empty targets are not supported."),
	providers.CanUseSSHFP:            providers.Can(),
	providers.CanUseTLSA:             providers.Can(),
	providers.CanUseTXTMulti:         providers.Cannot("INWX only supports a single entry for TXT records"),
	providers.CanAutoDNSSEC:          providers.Unimplemented("Supported by INWX but not implemented yet."),
	providers.DocOfficiallySupported: providers.Cannot(),
	providers.DocDualHost:            providers.Can(),
	providers.DocCreateDomains:       providers.Unimplemented("Supported by INWX but not implemented yet."),
	providers.CanGetZones:            providers.Can(),
	providers.CanUseAzureAlias:       providers.Cannot(),
}

// init registers the registrar and the domain service provider with dnscontrol.
func init() {
	providers.RegisterRegistrarType("INWX", newInwxReg)
	providers.RegisterDomainServiceProviderType("INWX", newInwxDsp, features)
}

// getOTP either returns the TOTPValue or uses TOTPKey and the current time to generate a valid TOTPValue.
func getOTP(TOTPValue string, TOTPKey string) (string, error) {
	if TOTPValue != "" {
		return TOTPValue, nil
	} else if TOTPKey != "" {
		tan, err := totp.GenerateCode(TOTPKey, time.Now())
		if err != nil {
			return "", fmt.Errorf("INWX: Unable to generate TOTP from totp-key: %v", err)
		}
		return tan, nil
	} else {
		return "", fmt.Errorf("INWX: two factor authentication required but no TOTP configured.")
	}
}

// loginHelper tries to login and then unlocks the account using two factor authentication if required.
func (api *InwxApi) loginHelper(TOTPValue string, TOTPKey string) error {
	resp, err := api.client.Account.Login()
	if err != nil {
		return fmt.Errorf("INWX: Unable to login")
	}

	switch TFA := resp.TFA; TFA {
	case "0":
		if TOTPKey != "" || TOTPValue != "" {
			fmt.Printf("INWX: Warning: no TOTP requested by INWX but totp/totp-key is present in `creds.json`\n")
		}
	case "GOOGLE-AUTH":
		tan, err := getOTP(TOTPValue, TOTPKey)
		if err != nil {
			return err
		}

		err = api.client.Account.Unlock(tan)
		if err != nil {
			return fmt.Errorf("INWX: Could not unlock account: %w.", err)
		}
	default:
		return fmt.Errorf("INWX: Unknown two factor authentication mode `%s` has been requested.", resp.TFA)
	}

	return nil
}

// newInwx initializes InwxApi and create a session.
func newInwx(m map[string]string) (*InwxApi, error) {
	username, password := m["username"], m["password"]
	TOTPValue, TOTPKey := m["totp"], m["totp-key"]
	sandbox := m["sandbox"] == "1"

	if username == "" {
		return nil, fmt.Errorf("INWX: username must be provided.")
	}
	if password == "" {
		return nil, fmt.Errorf("INWX: password must be provided.")
	}
	if TOTPValue != "" && TOTPKey != "" {
		return nil, fmt.Errorf("INWX: totp and totp-key must not be specified at the same time.")
	}

	opts := &goinwx.ClientOptions{Sandbox: sandbox}
	client := goinwx.NewClient(username, password, opts)
	api := &InwxApi{client: client, sandbox: sandbox}

	err := api.loginHelper(TOTPValue, TOTPKey)
	if err != nil {
		return nil, err
	}

	return api, nil
}

// newInwxReg is called to initialize the INWX registrar provider.
func newInwxReg(m map[string]string) (providers.Registrar, error) {
	return newInwx(m)
}

// new InwxDsp is called to initialize the INWX domain service provider.
func newInwxDsp(m map[string]string, metadata json.RawMessage) (providers.DNSServiceProvider, error) {
	return newInwx(m)
}

// makeNameserverRecordRequest is a helper function used to convert a RecordConfig to an INWX NS Record Request.
func makeNameserverRecordRequest(domain string, rec *models.RecordConfig) *goinwx.NameserverRecordRequest {
	content := rec.GetTargetField()

	req := &goinwx.NameserverRecordRequest{
		Domain:  domain,
		Type:    rec.Type,
		Content: content,
		Name:    rec.GetLabel(),
		TTL:     int(rec.TTL),
	}

	switch rType := rec.Type; rType {
	/* INWX is a little bit special for CNAME,NS,MX and SRV records:
	   The API will not accept any target with a final dot but will
	   instead always add this final dot internally.
	   Records with empty targets (i.e. records with target ".")
	   are not allowed.
	*/
	case "CNAME", "NS":
		req.Content = content[:len(content)-1]
	case "MX":
		req.Priority = int(rec.MxPreference)
		req.Content = content[:len(content)-1]
	case "SRV":
		req.Priority = int(rec.SrvPriority)
		req.Content = fmt.Sprintf("%d %d %v", rec.SrvWeight, rec.SrvPort, content[:len(content)-1])
	default:
		req.Content = rec.GetTargetCombined()
	}

	return req
}

// createRecord is used by GetDomainCorrections to create a new record.
func (api *InwxApi) createRecord(domain string, rec *models.RecordConfig) error {
	req := makeNameserverRecordRequest(domain, rec)
	_, err := api.client.Nameservers.CreateRecord(req)
	return err
}

// updateRecord is used by GetDomainCorrections to update an existing record.
func (api *InwxApi) updateRecord(RecordID int, rec *models.RecordConfig) error {
	req := makeNameserverRecordRequest("", rec)
	err := api.client.Nameservers.UpdateRecord(RecordID, req)
	return err
}

// deleteRecord is used by GetDomainCorrections to delete a record.
func (api *InwxApi) deleteRecord(RecordID int) error {
	return api.client.Nameservers.DeleteRecord(RecordID)
}

// GetDomainCorrections finds the currently existing records and returns the corrections required to update them.
func (api *InwxApi) GetDomainCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	dc.Punycode()

	foundRecords, err := api.GetZoneRecords(dc.Name)
	if err != nil {
		return nil, err
	}

	models.PostProcessRecords(foundRecords)

	differ := diff.New(dc)
	_, create, del, mod := differ.IncrementalDiff(foundRecords)
	corrections := []*models.Correction{}

	for _, d := range create {
		des := d.Desired
		corrections = append(corrections, &models.Correction{
			Msg: d.String(),
			F:   func() error { return api.createRecord(dc.Name, des) },
		})
	}
	for _, d := range del {
		existingID := d.Existing.Original.(goinwx.NameserverRecord).ID
		corrections = append(corrections, &models.Correction{
			Msg: d.String(),
			F:   func() error { return api.deleteRecord(existingID) },
		})
	}
	for _, d := range mod {
		rec := d.Desired
		existingID := d.Existing.Original.(goinwx.NameserverRecord).ID
		corrections = append(corrections, &models.Correction{
			Msg: d.String(),
			F:   func() error { return api.updateRecord(existingID, rec) },
		})
	}

	return corrections, nil
}

// GetNameservers returns the default nameservers for INWX.
func (api *InwxApi) GetNameservers(domain string) ([]*models.Nameserver, error) {
	if api.sandbox {
		return models.ToNameservers(InwxSandboxDefaultNs)
	} else {
		return models.ToNameservers(InwxDefaultNs)
	}
}

// GetZoneRecords receives the current records from Inwx and converts them to models.RecordConfig.
func (api *InwxApi) GetZoneRecords(domain string) (models.Records, error) {
	info, err := api.client.Nameservers.Info(&goinwx.NameserverInfoRequest{Domain: domain})
	if err != nil {
		return nil, err
	}

	var records = []*models.RecordConfig{}

	for _, record := range info.Records {
		if record.Type == "SOA" {
			continue
		}

		/* INWX is a little bit special for CNAME,NS,MX and SRV records:
		   The API will not accept any target with a final dot but will
		   instead always add this final dot internally.
		   Records with empty targets (i.e. records with target ".")
		   are not allowed.
		*/
		if record.Type == "CNAME" || record.Type == "MX" || record.Type == "NS" || record.Type == "SRV" {
			record.Content = record.Content + "."
		}

		rc := &models.RecordConfig{
			TTL:      uint32(record.TTL),
			Original: record,
		}
		rc.SetLabelFromFQDN(record.Name, domain)

		switch rType := record.Type; rType {
		case "MX":
			err = rc.SetTargetMX(uint16(record.Priority), record.Content)
		case "SRV":
			err = rc.SetTargetSRVPriorityString(uint16(record.Priority), record.Content)
		default:
			err = rc.PopulateFromString(rType, record.Content, domain)
		}

		if err != nil {
			panic(fmt.Errorf("INWX: unparsable record received: %w", err))
		}

		records = append(records, rc)
	}

	return records, nil
}

// updateNameservers is used by GetRegistrarCorrections to update the domain's nameservers.
func (api *InwxApi) updateNameservers(ns []string, domain string) func() error {
	return func() error {
		request := &goinwx.DomainUpdateRequest{
			Domain:      domain,
			Nameservers: ns,
		}

		_, err := api.client.Domains.Update(request)
		return err
	}
}

// GetRegistrarCorrections is part of the registrar provider and determines if the nameservers have to be updated.
func (api *InwxApi) GetRegistrarCorrections(dc *models.DomainConfig) ([]*models.Correction, error) {
	info, err := api.client.Domains.Info(dc.Name, 0)
	if err != nil {
		return nil, err
	}

	sort.Strings(info.Nameservers)
	foundNameservers := strings.Join(info.Nameservers, ",")
	expected := []string{}
	for _, ns := range dc.Nameservers {
		expected = append(expected, ns.Name)
	}
	sort.Strings(expected)
	expectedNameservers := strings.Join(expected, ",")

	if foundNameservers != expectedNameservers {
		return []*models.Correction{
			{
				Msg: fmt.Sprintf("Update nameservers %s -> %s", foundNameservers, expectedNameservers),
				F:   api.updateNameservers(expected, dc.Name),
			},
		}, nil
	}
	return nil, nil
}
