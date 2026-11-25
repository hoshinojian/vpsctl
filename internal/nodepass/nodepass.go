// Package nodepass 保管 vpsctl 给每台节点生成的随机 SSH 密码。
//
// 建机时每台节点拿一个互不相同的随机密码（经 cloud-init 注入），记入账号配置旁边的
// 密码库 node-passwords.json（0600）；导出 NMS 载荷时逐台取回。密码只进本地文件：
// 不进结果 JSON、不回传管理台，要看用 `vpsctl password`。
package nodepass

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/hoshinojian/vpsctl/internal/fleet"
	"github.com/hoshinojian/vpsctl/internal/provider"
)

// FileName 是密码库文件名，与 accounts.json 放在同一目录。
const FileName = "node-passwords.json"

// Entry 是一台节点的登录凭据。
type Entry struct {
	User      string    `json:"user"`
	Password  string    `json:"password"`
	CreatedAt time.Time `json:"created_at"`
}

// PathFor 由账号配置路径推出密码库路径（同目录）。
func PathFor(accountsPath string) string {
	return filepath.Join(filepath.Dir(accountsPath), FileName)
}

// passwordLength 是随机密码长度；只用字母数字——它会以明文落进 cloud-config 的块标量
// 与单引号标量，避开引号、反斜杠、冒号这些需要转义的字符。
const passwordLength = 24

const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// Generate 生成一个随机密码：大小写字母、数字三类都有，其余位置随机后再整体打乱。
func Generate() (string, error) {
	pw := make([]byte, passwordLength)
	// 三类在 alphabet 里各占一段——大写 [0,26)、小写 [26,52)、数字 [52,62)：先各放一个，
	// 保证三类齐全
	for i, class := range [][2]int{{0, 26}, {26, 26}, {52, 10}} {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(class[1])))
		if err != nil {
			return "", fmt.Errorf("生成随机密码: %w", err)
		}
		pw[i] = alphabet[class[0]+int(n.Int64())]
	}
	for i := 3; i < len(pw); i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			return "", fmt.Errorf("生成随机密码: %w", err)
		}
		pw[i] = alphabet[n.Int64()]
	}
	// Fisher-Yates 打乱：别让"前三位各占一类"成为可预测的规律
	for i := len(pw) - 1; i > 0; i-- {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", fmt.Errorf("生成随机密码: %w", err)
		}
		j := int(n.Int64())
		pw[i], pw[j] = pw[j], pw[i]
	}
	return string(pw), nil
}

// Spec 是某个账号建机注入密码所需的账号侧信息。
type Spec struct {
	User           string // 缺省 root
	ChpasswdCompat bool   // 见 fleet.CloudInitParams.ChpasswdCompat
}

// Provision 给计划里的每台节点生成一个随机密码，把各自的 cloud-init 放进
// o.UserDataByNode（优先级高于按账号 / 全局的 user-data），同一份凭据的结构化形式放进
// o.LoginByNode（给没有 cloud-init 的提供商用），返回"节点名 → 凭据"。
// 只生成、不落库：要等建机结果出来、拿到实例 ID 才能入库（见 Store.RecordCreated）。
func Provision(o *fleet.Options, specs map[string]Spec) (map[string]Entry, error) {
	generated := make(map[string]Entry)
	if o.UserDataByNode == nil {
		o.UserDataByNode = map[string]string{}
	}
	if o.LoginByNode == nil {
		o.LoginByNode = map[string]provider.Login{}
	}
	for _, ac := range o.Clients {
		spec := specs[ac.Name]
		user := spec.User
		if user == "" {
			user = "root"
		}
		for _, name := range o.NamesFor(ac.Name) {
			pw, err := Generate()
			if err != nil {
				return nil, err
			}
			ud, err := fleet.CloudInitSpec(fleet.CloudInitParams{
				User: user, Password: pw, SSHPort: o.SSHPort, Tunnel443: o.Tunnel443,
				AuthorizedKeys: o.AuthorizedKeys, ChpasswdCompat: spec.ChpasswdCompat,
			})
			if err != nil {
				return nil, fmt.Errorf("账号 %s: %w", ac.Name, err)
			}
			o.UserDataByNode[name] = ud
			o.LoginByNode[name] = provider.Login{User: user, Password: pw}
			generated[name] = Entry{User: user, Password: pw}
		}
	}
	return generated, nil
}

// Store 是密码库：account/实例ID → 凭据。每次操作都现读现写磁盘上的文件——CLI 与
// 管理台是两个进程，各持一份内存副本会互相覆盖对方新写的条目。
type Store struct {
	mu   sync.Mutex
	path string
	now  func() time.Time
}

// Open 返回指向 path 的密码库；文件还不存在时视为空库，第一次写入时才创建。
func Open(path string) *Store { return &Store{path: path, now: time.Now} }

type file struct {
	Nodes map[string]Entry `json:"nodes"`
}

// Key 是一台节点在密码库里的键（账号 / 实例 ID）。
func Key(account, id string) string { return account + "/" + id }

// Load 读出全部记录；密码库文件不存在按空库处理（还没建过节点、或存量部署只有
// 账号级密码）。文件损坏则报错——这时宁可导出失败，也不能悄悄回落到错的密码。
func (s *Store) Load() (map[string]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return nil, err
	}
	return f.Nodes, nil
}

func (s *Store) load() (file, error) {
	var f file
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return f, fmt.Errorf("读取密码库 %s: %w", s.path, err)
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return f, fmt.Errorf("解析密码库 %s: %w", s.path, err)
	}
	return f, nil
}

// CheckWritable 在建机前确认密码库写得进去：真建完才发现写不进去，密码就只留在内存里了。
func (s *Store) CheckWritable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.load()
	if err != nil {
		return err
	}
	return s.save(f)
}

// RecordCreated 把建机产物落库：generated 是"节点名 → 凭据"，created 里每台节点按
// 账号 / 实例 ID 入库；没建出来的（不在 created 里）不入库。
func (s *Store) RecordCreated(created []fleet.ServerJSON, generated map[string]Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(generated) == 0 || len(created) == 0 {
		return nil
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	if f.Nodes == nil {
		f.Nodes = map[string]Entry{}
	}
	for _, sv := range created {
		e, ok := generated[sv.Name]
		if !ok || sv.ID == "" {
			continue
		}
		e.CreatedAt = s.now().UTC().Truncate(time.Second)
		f.Nodes[Key(sv.Account, sv.ID)] = e
	}
	return s.save(f)
}

// ForgetAll 删掉一批节点的凭据记录（节点已销毁）；本就没有的静默跳过，密码库文件
// 还不存在时什么都不做（不会为了清理而建出一个空库）。
func (s *Store) ForgetAll(targets []fleet.Target) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(targets) == 0 {
		return nil
	}
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	f, err := s.load()
	if err != nil {
		return err
	}
	changed := false
	for _, t := range targets {
		if _, ok := f.Nodes[Key(t.Account, t.ID)]; ok {
			delete(f.Nodes, Key(t.Account, t.ID))
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save(f)
}

// RecordOrPrint 建机之后把随机密码落库。落库失败时这些密码只存在于当前进程的内存里，
// 节点一旦让出就再也登不进——所以先把它们原样打到 emergency（通常是 stderr）再报错。
func (s *Store) RecordOrPrint(created []fleet.ServerJSON, generated map[string]Entry, emergency io.Writer) error {
	err := s.RecordCreated(created, generated)
	if err == nil || emergency == nil {
		return err
	}
	fmt.Fprintf(emergency, "\n密码库写入失败（%v）！以下节点的密码只显示这一次，请立即保存：\n", err)
	for _, sv := range created {
		if e, ok := generated[sv.Name]; ok {
			fmt.Fprintf(emergency, "  %s\t%s\tuser=%s\tpassword=%s\n", Key(sv.Account, sv.ID), sv.Name, e.User, e.Password)
		}
	}
	return err
}

// save 先写同目录的临时文件再改名：中途失败不会留下半截密码库。
func (s *Store) save(f file) error {
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("编码密码库: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("写入密码库 %s: %w", s.path, err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("替换密码库 %s: %w", s.path, err)
	}
	return nil
}
