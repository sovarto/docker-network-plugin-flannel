package networking

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"log"
)

type IptablesRule struct {
	Table    string
	Chain    string
	RuleSpec []string
}

func ApplyIpTablesRules(rules []IptablesRule, action string) error {
	iptablev4, err := iptables.New()
	if err != nil {
		log.Printf("Error initializing iptables: %v", err)
		return err
	}

	var a func(table string, chain string, rulespec ...string) error
	switch action {
	case "create":
		a = iptablev4.AppendUnique
		break
	case "delete":
		a = iptablev4.Delete
		break
	default:
		return fmt.Errorf("invalid action. specify 'create' or 'delete'")
	}

	for _, rule := range rules {
		if err := a(rule.Table, rule.Chain, rule.RuleSpec...); err != nil {
			log.Printf("Error applying iptables rule in table %s, chain %s: %v", rule.Table, rule.Chain, err)
			return err
		}
	}

	return nil
}
