package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type Menu struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	URL        string `json:"url"`
	Icon       string `json:"icon_svg"`
	Visibility string `json:"visibility"`
	SortOrder  int    `json:"sort_order"`
}

func cleanSVG(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > 10*1024 {
		return "", fmt.Errorf("请选择不超过 10KB 的 SVG 文件")
	}
	tags := strings.Fields("svg g path circle ellipse line polyline polygon rect title desc")
	attrs := strings.Fields("xmlns viewBox width height x y x1 y1 x2 y2 cx cy r rx ry d points fill stroke stroke-width stroke-linecap stroke-linejoin fill-rule clip-rule opacity fill-opacity stroke-opacity transform role aria-label")
	allowed := func(list []string, s string) bool {
		for _, v := range list {
			if v == s {
				return true
			}
		}
		return false
	}
	d := xml.NewDecoder(strings.NewReader(raw))
	var b bytes.Buffer
	enc := xml.NewEncoder(&b)
	depth := 0
	roots := 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("SVG 文件格式无效")
		}
		switch t := token.(type) {
		case xml.StartElement:
			if !allowed(tags, t.Name.Local) || (t.Name.Space != "" && t.Name.Space != "http://www.w3.org/2000/svg") {
				return "", fmt.Errorf("SVG 只支持静态矢量图形，请移除脚本、图片和外部引用")
			}
			if depth == 0 {
				roots++
				if t.Name.Local != "svg" || roots != 1 {
					return "", fmt.Errorf("SVG 需要一个 svg 根元素")
				}
			}
			depth++
			if depth > 32 {
				return "", fmt.Errorf("SVG 嵌套过深")
			}
			t.Name.Space = ""
			for i := range t.Attr {
				a := &t.Attr[i]
				v := strings.ToLower(a.Value)
				if !allowed(attrs, a.Name.Local) || a.Name.Space != "" || strings.Contains(v, "url(") || strings.Contains(v, "javascript:") || strings.ContainsAny(a.Value, "\r\n") {
					return "", fmt.Errorf("SVG 包含不支持的属性或外部引用")
				}
				if a.Name.Local == "xmlns" && a.Value != "http://www.w3.org/2000/svg" {
					return "", fmt.Errorf("SVG 命名空间无效")
				}
			}
			if err := enc.EncodeToken(t); err != nil {
				return "", err
			}
		case xml.EndElement:
			depth--
			t.Name.Space = ""
			if err := enc.EncodeToken(t); err != nil {
				return "", err
			}
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(t)) != "" {
				return "", fmt.Errorf("SVG 根元素外不能包含文本")
			}
			if err := enc.EncodeToken(t); err != nil {
				return "", err
			}
		case xml.Comment:
		case xml.ProcInst:
			if t.Target != "xml" {
				return "", fmt.Errorf("SVG 不支持处理指令")
			}
		default:
			return "", fmt.Errorf("SVG 不支持文档类型或实体声明")
		}
	}
	if roots != 1 || depth != 0 {
		return "", fmt.Errorf("SVG 文件格式无效")
	}
	if err := enc.Flush(); err != nil {
		return "", err
	}
	if b.Len() > 10*1024 {
		return "", fmt.Errorf("规范化后的 SVG 超过 10KB，请精简图形")
	}
	return b.String(), nil
}

type Gateway struct {
	base   string
	client *http.Client
}

var errForbidden = errors.New("此功能仅限管理员")

func (g *Gateway) call(ctx context.Context, method, path string, body any, headers http.Header, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(g.base, "/")+path, reader)
	if err != nil {
		return fmt.Errorf("主服务地址无效")
	}
	for _, k := range []string{"Authorization", "X-Api-Key", "User-Agent", "X-Forwarded-For", "X-Real-Ip"} {
		if v := headers.Get(k); v != "" {
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接主服务")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("主服务返回 HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&envelope); err != nil {
		return fmt.Errorf("主服务响应格式无效")
	}
	if envelope.Code != 0 {
		return fmt.Errorf("主服务拒绝此操作")
	}
	if out != nil {
		return json.Unmarshal(envelope.Data, out)
	}
	return nil
}

func (g *Gateway) admin(ctx context.Context, headers http.Header) error {
	if !strings.HasPrefix(headers.Get("Authorization"), "Bearer ") || len(headers.Get("Authorization")) < 10 {
		return fmt.Errorf("请先在主站登录管理员账户")
	}
	var user struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/v1/auth/me", nil, headers, &user); err != nil {
		return fmt.Errorf("登录已失效或主服务无法验证，请重新登录")
	}
	if user.Role != "admin" || (user.Status != "" && user.Status != "active") {
		return errForbidden
	}
	return nil
}

func (g *Gateway) menu(ctx context.Context, headers http.Header, desired Menu) (Menu, error) {
	u, err := url.Parse(desired.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/account-eval/" {
		return Menu{}, fmt.Errorf("入口地址必须是主站的 http(s) 地址加 /account-eval/")
	}
	var settings struct {
		Items []json.RawMessage `json:"custom_menu_items"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/v1/admin/settings", nil, headers, &settings); err != nil {
		return Menu{}, err
	}
	var old Menu
	index := -1
	for i, b := range settings.Items {
		var m Menu
		if err := json.Unmarshal(b, &m); err != nil {
			return Menu{}, fmt.Errorf("现有菜单格式无效")
		}
		if m.ID == "account-eval" {
			old = m
			index = i
			break
		}
	}
	if desired.Icon == "" {
		desired.Icon = old.Icon
		if desired.Icon == "" {
			b, _ := assets.ReadFile("web/icon.svg")
			desired.Icon = string(b)
		}
	}
	desired.Icon, err = cleanSVG(desired.Icon)
	if err != nil {
		return Menu{}, err
	}
	if desired.Label == "" {
		desired.Label = old.Label
		if desired.Label == "" {
			desired.Label = "账户测评"
		}
	}
	if len(desired.Label) > 50 {
		return Menu{}, fmt.Errorf("入口名称最多 50 字节")
	}
	desired.ID = "account-eval"
	desired.Visibility = "admin"
	desired.SortOrder = old.SortOrder
	if index < 0 {
		desired.SortOrder = len(settings.Items) + 1
	}
	data, err := json.Marshal(desired)
	if err != nil {
		return Menu{}, err
	}
	if index < 0 {
		settings.Items = append(settings.Items, data)
	} else {
		settings.Items[index] = data
	}
	if err := g.call(ctx, http.MethodPut, "/api/v1/admin/settings", map[string]any{"custom_menu_items": settings.Items}, headers, nil); err != nil {
		return Menu{}, err
	}
	return desired, nil
}
