// Package ui 的 skills.go 实现 /skills 命令族：GitHub skills 仓库的浏览、
// 安装、激活、卸载，以及已安装技能列表展示。
//
// 设计见 PRD/skills-management-prd.md。复用 internal/skills 包的 Market。
package ui

import (
	"fmt"
	"strconv"

	"LongCat-frontend/internal/frontend"
	"LongCat-frontend/internal/skills"
)

// getMarket 懒加载 skills Market。
func (t *TUI) getMarket() *skills.Market {
	if t.market == nil {
		m, err := skills.NewMarket()
		if err != nil {
			t.errorf("初始化 skills 市场失败: %v", err)
			return nil
		}
		t.market = m
	}
	return t.market
}

func (t *TUI) warnf(format string, a ...any) {
	fmt.Print(t.c(t.pal.warn, "  ⚠ ") + fmt.Sprintf(format, a...) + "\n")
}

// cmdSkills /skills 命令入口。
func (t *TUI) cmdSkills(args []string) {
	if len(args) == 0 {
		t.skillsList()
		return
	}
	switch args[0] {
	case "repo":
		t.cmdSkillsRepo(args[1:])
	case "browse":
		if len(args) < 2 {
			t.errorf("用法: /skills browse <owner/repo>")
			return
		}
		t.skillsBrowse(args[1])
	case "install":
		if len(args) < 3 {
			t.errorf("用法: /skills install <owner/repo> <name|编号>")
			return
		}
		t.skillsInstall(args[1], args[2])
	case "uninstall", "remove":
		if len(args) < 2 {
			t.errorf("用法: /skills uninstall <name>")
			return
		}
		t.skillsUninstall(args[1])
	case "use":
		if len(args) < 2 {
			t.errorf("用法: /skills use <name|编号>")
			return
		}
		t.skillsUse(args[1])
	case "refresh":
		if len(args) < 2 {
			t.errorf("用法: /skills refresh <owner/repo>")
			return
		}
		t.skillsRefresh(args[1])
	case "installed":
		t.skillsList()
	case "auth":
		t.skillsAuth()
	case "help":
		t.skillsHelp()
	default:
		// /skills <name> 等同于 use
		t.skillsUse(args[0])
	}
}

func (t *TUI) skillsHelp() {
	fmt.Println()
	fmt.Println("  " + t.c(bold, "skills 管理命令"))
	rows := [][2]string{
		{"/skills", "列出已安装技能"},
		{"/skills use <name|n>", "选择激活某个技能"},
		{"/skills repo add <o/r>", "添加 GitHub skills 仓库"},
		{"/skills repo list", "列出已收藏仓库"},
		{"/skills browse <o/r>", "浏览仓库的 skills"},
		{"/skills install <o/r> <name>", "安装某 skill"},
		{"/skills uninstall <name>", "卸载技能"},
		{"/skills refresh <o/r>", "刷新仓库缓存"},
		{"/skills auth", "检查 gh 登录状态"},
	}
	for _, r := range rows {
		fmt.Printf("  %s%s\n", t.c(t.pal.primary, fmt.Sprintf("%-30s", r[0])), t.c(t.pal.muted, r[1]))
	}
}

// skillsList 列出已安装技能（带编号 + active 标记）。
func (t *TUI) skillsList() {
	m := t.getMarket()
	if m == nil {
		return
	}
	fmt.Println()
	fmt.Println("  " + t.c(bold, "已安装技能"))
	installed := m.ListInstalled()
	if len(installed) == 0 {
		t.errorf("暂无已安装技能")
		fmt.Println("  " + t.c(dim+t.pal.muted, "添加仓库: /skills repo add <owner/repo> · 浏览: /skills browse <owner/repo>"))
		return
	}
	for i, name := range installed {
		mark := t.c(dim+t.pal.muted, " ")
		if t.session.ActiveSkill == name {
			mark = t.c(t.pal.ok, "●")
		}
		title := name
		for _, s := range t.session.Skills {
			if s.Name == name {
				title = s.Title
				break
			}
		}
		fmt.Printf("  %s %s %s %s\n", mark,
			t.c(t.pal.primary, fmt.Sprintf("%2d.", i+1)),
			t.c(bold, title),
			t.c(dim+t.pal.muted, "("+name+")"))
	}
	fmt.Println()
	fmt.Println("  " + t.c(dim+t.pal.muted, "激活: /skills use <name|编号> · 浏览仓库: /skills browse <owner/repo>"))
}

// skillsUse 选择激活某个已安装技能（支持名称或编号）。
func (t *TUI) skillsUse(name string) {
	m := t.getMarket()
	if m == nil {
		return
	}
	if n, err := strconv.Atoi(name); err == nil {
		installed := m.ListInstalled()
		if n < 1 || n > len(installed) {
			t.errorf("编号超出范围（1-%d）", len(installed))
			return
		}
		name = installed[n-1]
	}
	found := false
	for _, s := range t.session.Skills {
		if s.Name == name {
			found = true
			break
		}
	}
	if !found {
		t.errorf("技能 %s 未安装", name)
		return
	}
	t.session.ActiveSkill = name
	t.okf("已激活技能: %s（下次对话生效，输入消息即可使用）", name)
}

// skillsBrowse 浏览仓库的 skills（未添加则自动拉取）。
func (t *TUI) skillsBrowse(repo string) {
	m := t.getMarket()
	if m == nil {
		return
	}
	if _, err := m.GetRepo(repo); err != nil {
		t.okf("首次浏览，正在拉取 %s ...", repo)
		if err := m.AddRepo(repo); err != nil {
			t.errorf("拉取失败: %v", err)
			return
		}
	}
	list, err := m.BrowseRepo(repo)
	if err != nil {
		t.errorf("浏览失败: %v", err)
		return
	}
	fmt.Println()
	if r, err := m.GetRepo(repo); err == nil {
		fmt.Printf("  %s %s\n",
			t.c(bold+t.pal.primary, r.Owner+"/"+r.Name),
			t.c(dim+t.pal.muted, fmt.Sprintf("★ %d · %s", r.Stars, r.Description)))
	}
	if len(list) == 0 {
		t.errorf("仓库内未找到 skills/<name>/SKILL.md（约定格式见 PRD）")
		return
	}
	fmt.Println("  " + t.c(bold, "Skills"))
	for i, s := range list {
		mark := t.c(dim+t.pal.muted, " ")
		if m.IsInstalled(s.Name) {
			mark = t.c(t.pal.ok, "✓")
		}
		fmt.Printf("  %s %s %s %s\n", mark,
			t.c(t.pal.primary, fmt.Sprintf("%2d.", i+1)),
			t.c(bold, s.Title),
			t.c(dim+t.pal.muted, s.Description))
	}
	fmt.Println()
	fmt.Println("  " + t.c(dim+t.pal.muted, "安装: /skills install "+repo+" <name|编号>"))
}

func (t *TUI) skillsInstall(repo, name string) {
	m := t.getMarket()
	if m == nil {
		return
	}
	if n, err := strconv.Atoi(name); err == nil {
		if list, err := m.BrowseRepo(repo); err == nil && n >= 1 && n <= len(list) {
			name = list[n-1].Name
		}
	}
	if m.IsInstalled(name) {
		t.warnf("技能 %s 已安装，覆盖更新...", name)
	}
	if err := m.Install(repo, name); err != nil {
		t.errorf("安装失败: %v", err)
		return
	}
	t.reloadSkills()
	t.okf("已安装技能: %s（已加入会话）", name)
}

func (t *TUI) skillsUninstall(name string) {
	m := t.getMarket()
	if m == nil {
		return
	}
	if !m.IsInstalled(name) {
		t.errorf("技能 %s 未安装", name)
		return
	}
	if err := m.Uninstall(name); err != nil {
		t.errorf("卸载失败: %v", err)
		return
	}
	var filtered []frontend.Skill
	for _, s := range t.session.Skills {
		if s.Name != name {
			filtered = append(filtered, s)
		}
	}
	t.session.Skills = filtered
	if t.session.ActiveSkill == name {
		t.session.ActiveSkill = ""
	}
	t.okf("已卸载技能: %s", name)
}

func (t *TUI) skillsRefresh(repo string) {
	m := t.getMarket()
	if m == nil {
		return
	}
	t.okf("刷新 %s ...", repo)
	if err := m.Refresh(repo); err != nil {
		t.errorf("刷新失败: %v", err)
		return
	}
	t.okf("已刷新: %s", repo)
}

func (t *TUI) skillsAuth() {
	user, err := skills.AuthStatus()
	if err != nil {
		t.errorf("%v", err)
		return
	}
	if user == "" {
		t.warnf("gh 已安装但未登录")
		return
	}
	t.okf("gh 已登录: %s", user)
}

func (t *TUI) cmdSkillsRepo(args []string) {
	if len(args) == 0 {
		t.errorf("用法: /skills repo <add|list|remove> ...")
		return
	}
	m := t.getMarket()
	if m == nil {
		return
	}
	switch args[0] {
	case "add":
		if len(args) < 2 {
			t.errorf("用法: /skills repo add <owner/repo>")
			return
		}
		t.okf("正在拉取 %s ...", args[1])
		if err := m.AddRepo(args[1]); err != nil {
			t.errorf("添加失败: %v", err)
			return
		}
		t.okf("已添加仓库: %s", args[1])
	case "list", "ls":
		repos, err := m.ListRepos()
		if err != nil {
			t.errorf("%v", err)
			return
		}
		fmt.Println()
		if len(repos) == 0 {
			t.errorf("暂无仓库，用 /skills repo add <owner/repo> 添加")
			return
		}
		fmt.Println("  " + t.c(bold, "已收藏仓库"))
		for _, r := range repos {
			fmt.Printf("  %s %s\n",
				t.c(bold, r.URL),
				t.c(dim+t.pal.muted, fmt.Sprintf("★ %d · %s · %s", r.Stars, r.Owner, r.Description)))
		}
	case "remove", "rm":
		if len(args) < 2 {
			t.errorf("用法: /skills repo remove <owner/repo>")
			return
		}
		if err := m.RemoveRepo(args[1]); err != nil {
			t.errorf("%v", err)
			return
		}
		t.okf("已删除仓库: %s", args[1])
	default:
		t.errorf("未知子命令: /skills repo %s", args[0])
	}
}

// reloadSkills 重新加载用户级 skills 并合并到 session（覆盖同名，新增追加）。
func (t *TUI) reloadSkills() {
	st, err := skills.NewStore()
	if err != nil {
		return
	}
	userSkills, err := frontend.LoadSkills(st.Dir())
	if err != nil {
		return
	}
	byName := map[string]int{}
	for i, s := range t.session.Skills {
		byName[s.Name] = i
	}
	for _, s := range userSkills {
		if idx, ok := byName[s.Name]; ok {
			t.session.Skills[idx] = s
		} else {
			t.session.Skills = append(t.session.Skills, s)
		}
	}
}
