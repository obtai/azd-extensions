package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/postgresql/armpostgresqlflexibleservers/v4"

	"github.com/obtai/azd-extensions/db/internal/azure"
	"github.com/obtai/azd-extensions/db/internal/ui"
)

// checkFirewall is advice, never a gate: a server can be reachable for reasons
// this check cannot see — a VPN, a private endpoint, an IP lookup that failed —
// so every path here ends by letting the connection be attempted.
func checkFirewall(ctx context.Context, u ui.UI, clients *azure.Clients, resourceGroup, server, user string) {
	rules, err := ui.Loading(ctx, u, "Checking the firewall…", func(ctx context.Context) ([]*armpostgresqlflexibleservers.FirewallRule, error) {
		return clients.FirewallRules(ctx, resourceGroup, server)
	})
	if err != nil {
		// Reading the rules needs a role that connecting does not. Not being
		// allowed to look is not a reason to stop.
		return
	}
	if coversInternet(rules) {
		return
	}

	address, err := publicIP(ctx)
	if err != nil || covers(rules, address) {
		return
	}

	add, err := ui.Confirm(ctx, u, fmt.Sprintf("No firewall rule covers %s. Add one to %s?", address, server))
	if err != nil || !add {
		ui.Note("No firewall rule on %s covers %s. If the connection hangs or is refused:\n"+
			"  az postgres flexible-server firewall-rule create -g %s -s %s "+
			"--rule-name %s --start-ip-address %s --end-ip-address %s",
			server, address, resourceGroup, server, ruleName(user), address, address)
		return
	}

	if err := clients.AddFirewallRule(ctx, resourceGroup, server, ruleName(user), address.String()); err != nil {
		ui.Note("%v", err)
		return
	}
	ui.Note("Added %s for %s. A new rule can take a moment to take effect.", ruleName(user), address)
}

// coversInternet reports whether a rule opens the server to everything, which
// is what the OBT blueprint does and what makes the IP lookup unnecessary.
func coversInternet(rules []*armpostgresqlflexibleservers.FirewallRule) bool {
	for _, rule := range rules {
		start, end, ok := span(rule)
		if !ok {
			continue
		}
		if start.Compare(netip.AddrFrom4([4]byte{0, 0, 0, 0})) == 0 &&
			end.Compare(netip.AddrFrom4([4]byte{255, 255, 255, 255})) == 0 {
			return true
		}
	}
	return false
}

// covers reports whether any rule admits one address.
func covers(rules []*armpostgresqlflexibleservers.FirewallRule, address netip.Addr) bool {
	for _, rule := range rules {
		start, end, ok := span(rule)
		if !ok {
			continue
		}
		// The Azure-services marker is a zero-length span at 0.0.0.0, so it
		// falls out of the comparison on its own — but only because no real
		// client address is 0.0.0.0.
		if start.Compare(address) <= 0 && end.Compare(address) >= 0 {
			return true
		}
	}
	return false
}

func span(rule *armpostgresqlflexibleservers.FirewallRule) (netip.Addr, netip.Addr, bool) {
	if rule == nil || rule.Properties == nil {
		return netip.Addr{}, netip.Addr{}, false
	}
	start, err := netip.ParseAddr(deref(rule.Properties.StartIPAddress))
	if err != nil {
		return netip.Addr{}, netip.Addr{}, false
	}
	end, err := netip.ParseAddr(deref(rule.Properties.EndIPAddress))
	if err != nil {
		return netip.Addr{}, netip.Addr{}, false
	}
	return start, end, true
}

// publicIP asks what the internet sees, the way the az CLI does.
func publicIP(ctx context.Context) (netip.Addr, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return netip.Addr{}, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return netip.Addr{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64))
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(strings.TrimSpace(string(body)))
}

var notRuleName = regexp.MustCompile(`[^A-Za-z0-9-]+`)

// ruleName is stable per person, so connecting from the same laptop twice
// updates one rule rather than littering the server with them.
func ruleName(user string) string {
	name := notRuleName.ReplaceAllString(user, "-")
	name = strings.Trim(name, "-")
	if name == "" {
		name = "user"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	return "azd-db-" + name
}
