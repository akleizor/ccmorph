package quota

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// A provider whose account balance can be read with the profile's own API key:
// a GET, no inference, nothing billed. Recognised by the base URL's host; an
// endpoint on no known host has no balance to show.
type balanceReader struct {
	hosts []string
	url   func(host string) string
	// bare: the key goes in Authorization without "Bearer " (Zhipu's billing API).
	bare  bool
	parse func(body []byte) (string, error)
}

var balanceReaders = []balanceReader{
	{hosts: []string{"api.deepseek.com"}, url: path("/user/balance"), parse: deepseekBalance},
	{hosts: []string{"openrouter.ai"}, url: path("/api/v1/credits"), parse: openrouterBalance},
	{hosts: []string{"api.moonshot.ai"}, url: path("/v1/users/me/balance"), parse: moonshotBalance("USD")},
	{hosts: []string{"api.moonshot.cn"}, url: path("/v1/users/me/balance"), parse: moonshotBalance("CNY")},
	{hosts: []string{"api.siliconflow.cn"}, url: path("/v1/user/info"), parse: siliconflowBalance("CNY")},
	{hosts: []string{"api.siliconflow.com"}, url: path("/v1/user/info"), parse: siliconflowBalance("USD")},
	{hosts: []string{"open.bigmodel.cn"}, url: path("/api/biz/account/query-customer-account-report"), bare: true, parse: zhipuBalance("CNY")},
	{hosts: []string{"api.z.ai"}, url: path("/api/biz/account/query-customer-account-report"), bare: true, parse: zhipuBalance("USD")},
	{hosts: []string{"api.stepfun.com"}, url: path("/v1/accounts"), parse: stepfunBalance("CNY")},
	{hosts: []string{"api.stepfun.ai"}, url: path("/v1/accounts"), parse: stepfunBalance("USD")},
}

func path(p string) func(string) string {
	return func(host string) string { return "https://" + host + p }
}

// balance is what is left behind an API key, and the row's error with any
// balance failure added. A base URL on no known provider host sends nothing and
// adds nothing: most gateways publish no balance.
func balance(client *http.Client, baseURL, key, prior string) (string, string) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || key == "" {
		return "", prior
	}
	host := strings.ToLower(u.Hostname())
	for _, r := range balanceReaders {
		for _, h := range r.hosts {
			if host != h {
				continue
			}
			got, err := readBalance(client, r, h, key)
			if err != nil {
				return "", strings.TrimPrefix(prior+"; balance: "+err.Error(), "; ")
			}
			return got, prior
		}
	}
	return "", prior
}

func readBalance(client *http.Client, r balanceReader, host, key string) (string, error) {
	req, _ := http.NewRequest("GET", r.url(host), nil)
	if r.bare {
		req.Header.Set("Authorization", key)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	body, status, err := fetch(client, req)
	switch {
	case err != nil:
		return "", fmt.Errorf("unreachable: %v", err)
	case status == 401 || status == 403:
		return "", fmt.Errorf("key refused by %s", host)
	case status != 200:
		return "", fmt.Errorf("%s answered %d", host, status)
	}
	return r.parse(body)
}

// num reads a JSON number or a numeric string.
func num(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(x, 64)
		return f, err == nil
	}
	return 0, false
}

func money(amount float64, currency string) string {
	switch currency {
	case "CNY":
		return fmt.Sprintf("¥%.2f", amount)
	case "USD":
		return fmt.Sprintf("$%.2f", amount)
	}
	return fmt.Sprintf("%.2f %s", amount, currency)
}

var errUnreadable = fmt.Errorf("unreadable balance reply")

// DeepSeek lists one wallet per currency; an empty wallet beside a funded one
// is noise, so it is shown only when every wallet is empty.
func deepseekBalance(body []byte) (string, error) {
	var b struct {
		Infos []struct {
			Currency string `json:"currency"`
			Total    any    `json:"total_balance"`
		} `json:"balance_infos"`
	}
	if json.Unmarshal(body, &b) != nil || len(b.Infos) == 0 {
		return "", errUnreadable
	}
	var all, funded []string
	for _, i := range b.Infos {
		v, ok := num(i.Total)
		if !ok {
			return "", errUnreadable
		}
		all = append(all, money(v, i.Currency))
		if v > 0 {
			funded = append(funded, money(v, i.Currency))
		}
	}
	if len(funded) > 0 {
		return strings.Join(funded, " "), nil
	}
	return strings.Join(all, " "), nil
}

// OpenRouter reports credits bought and spent; the balance is the difference.
func openrouterBalance(body []byte) (string, error) {
	var b struct {
		Data struct {
			Credits any `json:"total_credits"`
			Usage   any `json:"total_usage"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &b) != nil {
		return "", errUnreadable
	}
	c, ok1 := num(b.Data.Credits)
	u, ok2 := num(b.Data.Usage)
	if !ok1 || !ok2 {
		return "", errUnreadable
	}
	return money(c-u, "USD"), nil
}

func moonshotBalance(currency string) func([]byte) (string, error) {
	return func(body []byte) (string, error) {
		var b struct {
			Data struct {
				Available any `json:"available_balance"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &b) != nil {
			return "", errUnreadable
		}
		v, ok := num(b.Data.Available)
		if !ok {
			return "", errUnreadable
		}
		return money(v, currency), nil
	}
}

func siliconflowBalance(currency string) func([]byte) (string, error) {
	return func(body []byte) (string, error) {
		var b struct {
			Data struct {
				Total any `json:"totalBalance"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &b) != nil {
			return "", errUnreadable
		}
		v, ok := num(b.Data.Total)
		if !ok {
			return "", errUnreadable
		}
		return money(v, currency), nil
	}
}

// Zhipu answers 200 with success=false for a refused key.
func zhipuBalance(currency string) func([]byte) (string, error) {
	return func(body []byte) (string, error) {
		var b struct {
			Success *bool  `json:"success"`
			Msg     string `json:"msg"`
			Data    struct {
				Available any `json:"availableBalance"`
				Balance   any `json:"balance"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &b) != nil {
			return "", errUnreadable
		}
		if b.Success != nil && !*b.Success {
			return "", fmt.Errorf("key refused: %s", b.Msg)
		}
		v, ok := num(b.Data.Available)
		if !ok {
			v, ok = num(b.Data.Balance)
		}
		if !ok {
			return "", errUnreadable
		}
		return money(v, currency), nil
	}
}

func stepfunBalance(currency string) func([]byte) (string, error) {
	return func(body []byte) (string, error) {
		var b struct {
			Balance any `json:"balance"`
		}
		if json.Unmarshal(body, &b) != nil {
			return "", errUnreadable
		}
		v, ok := num(b.Balance)
		if !ok {
			return "", errUnreadable
		}
		return money(v, currency), nil
	}
}
