package main

import (
	"time"

	"github.com/nwverse/persona/sdk/go/wire"
)

// Templates: a space starts from one of these. Each brings its own object types (on top of the four built in) and a
// few synthetic examples so the views have something to show. Every example is made up; addresses are
// placeholders like <host-address>, links point at example.com.

type spaceTemplate struct {
	Key, Name, Icon, Description string
	Types                        []ObjectType
	seed                         func(st *State, sp *Space, now time.Time) error
}

func templates() []spaceTemplate {
	return []spaceTemplate{
		{Key: "lifeos", Name: "LifeOS 家庭空间", Icon: wire.NavIconCheck,
			Description: "家庭库存、餐食计划和家务：东西放在哪、什么快用完、这周吃什么、谁做什么家务。",
			Types:       lifeosTypes(), seed: seedLifeOS},
		{Key: "deals", Name: "薅羊毛空间", Icon: wire.NavIconMoney,
			Description: "好价线报、商家渠道和优惠券：记下原价、到手价、省了多少和什么时候失效。",
			Types:       dealsTypes(), seed: seedDeals},
		{Key: "cloud", Name: "云电脑", Icon: wire.NavIconCode,
			Description: "云主机节点和运维手册：哪台机器在哪个区域、多少钱一个月、什么时候到期、出问题照哪本手册做。",
			Types:       cloudTypes(), seed: seedCloud},
		{Key: "blank", Name: "自定义本体空间", Icon: wire.NavIconFolder,
			Description: "一个干净的空间：动态、点子、目标、资源库四个核心视图都在，对象类型由你自己注册。",
			seed:        func(*State, *Space, time.Time) error { return nil }},
	}
}

func templateByKey(key string) (spaceTemplate, bool) {
	for _, t := range templates() {
		if t.Key == key {
			return t, true
		}
	}
	return spaceTemplate{}, false
}

func lifeosTypes() []ObjectType {
	return []ObjectType{
		{Key: "location", Label: "收纳位置", Icon: wire.NavIconFolder, PrimaryField: "room",
			Description: "家里放东西的地方：冰箱、储物柜、药箱。",
			Fields: []FieldDef{
				{Key: "room", Label: "房间", Kind: KindText},
				{Key: "note", Label: "备注", Kind: KindText},
			}},
		{Key: "inventory_item", Label: "家庭库存", Icon: wire.NavIconTag, PrimaryField: "category",
			Description: "家里的库存物品：放在哪、还剩多少、什么时候过期、要不要补货。",
			Statuses:    []string{"在库", "快用完", "已用完"},
			Fields: []FieldDef{
				{Key: "category", Label: "分类", Kind: KindEnum, Options: []string{"食品", "日用品", "药品", "清洁", "其他"}, Required: true},
				{Key: "location", Label: "位置", Kind: KindRelation, TargetType: "location", RelationType: "located_in"},
				{Key: "quantity", Label: "数量", Kind: KindNumber},
				{Key: "unit", Label: "单位", Kind: KindText},
				{Key: "expiry_date", Label: "过期日", Kind: KindDate},
				{Key: "restock_needed", Label: "需要补货", Kind: KindBoolean},
			}},
		{Key: "meal_plan", Label: "餐食计划", Icon: wire.NavIconCalendar, PrimaryField: "dishes",
			Description: "哪天哪顿吃什么，要用到哪些食材。",
			Statuses:    []string{"计划中", "已完成"},
			Fields: []FieldDef{
				{Key: "date", Label: "日期", Kind: KindDate, Required: true},
				{Key: "meal", Label: "餐次", Kind: KindEnum, Options: []string{"早餐", "午餐", "晚餐", "加餐"}},
				{Key: "dishes", Label: "菜品", Kind: KindText},
				{Key: "ingredients", Label: "食材", Kind: KindText},
				{Key: "servings", Label: "份数", Kind: KindNumber, Unit: "人份"},
			}},
		{Key: "chore", Label: "家务事项", Icon: wire.NavIconCheck, PrimaryField: "area",
			Description: "家务：谁做、多久一次、下次什么时候、要多久。",
			Statuses:    []string{"待办", "进行中", "已完成"},
			Fields: []FieldDef{
				{Key: "assignee", Label: "负责人", Kind: KindText},
				{Key: "frequency", Label: "频率", Kind: KindEnum, Options: []string{"每天", "每周", "每月", "不定期"}},
				{Key: "due_date", Label: "下次日期", Kind: KindDate},
				{Key: "area", Label: "区域", Kind: KindText},
				{Key: "minutes", Label: "用时", Kind: KindNumber, Unit: "分钟"},
			}},
	}
}

func dealsTypes() []ObjectType {
	return []ObjectType{
		{Key: "merchant", Label: "商家/渠道", Icon: wire.NavIconPerson, PrimaryField: "platform",
			Description: "下单的商家或渠道：哪个平台、靠不靠谱。",
			Statuses:    []string{"常用", "观察", "拉黑"},
			Fields: []FieldDef{
				{Key: "platform", Label: "平台", Kind: KindText},
				{Key: "channel_url", Label: "入口", Kind: KindURL},
				{Key: "rating", Label: "评分", Kind: KindNumber, Unit: "分"},
				{Key: "note", Label: "备注", Kind: KindText},
			}},
		{Key: "deal", Label: "好价线报", Icon: wire.NavIconMoney, PrimaryField: "merchant_name",
			Description: "一条好价：原价、到手价、省多少、什么时候失效。",
			Statuses:    []string{"关注中", "已下单", "已失效"},
			Fields: []FieldDef{
				{Key: "platform", Label: "平台", Kind: KindText},
				{Key: "merchant_name", Label: "商家", Kind: KindText},
				{Key: "merchant", Label: "商家条目", Kind: KindRelation, TargetType: "merchant", RelationType: "belongs_to"},
				{Key: "original_price", Label: "原价", Kind: KindNumber, Unit: "元"},
				{Key: "deal_price", Label: "到手价", Kind: KindNumber, Unit: "元", Required: true},
				{Key: "saved_amount", Label: "省了", Kind: KindNumber, Unit: "元"},
				{Key: "valid_until", Label: "有效期至", Kind: KindDate},
				{Key: "url", Label: "链接", Kind: KindURL},
			}},
		{Key: "coupon", Label: "优惠券", Icon: wire.NavIconTag, PrimaryField: "code",
			Description: "领到的券：满多少减多少、什么时候过期。",
			Statuses:    []string{"可用", "已使用", "已过期"},
			Fields: []FieldDef{
				{Key: "platform", Label: "平台", Kind: KindText},
				{Key: "threshold", Label: "门槛", Kind: KindNumber, Unit: "元"},
				{Key: "discount", Label: "减", Kind: KindNumber, Unit: "元"},
				{Key: "expires_at", Label: "过期日", Kind: KindDate},
				{Key: "code", Label: "券码", Kind: KindText},
				{Key: "merchant", Label: "商家条目", Kind: KindRelation, TargetType: "merchant", RelationType: "belongs_to"},
			}},
	}
}

func cloudTypes() []ObjectType {
	return []ObjectType{
		{Key: "cloud_host", Label: "云主机节点", Icon: wire.NavIconCode, PrimaryField: "spec",
			Description: "一台云主机：服务商、区域、规格、系统、月费和到期日。",
			Statuses:    []string{"运行中", "已停止", "已释放"},
			Fields: []FieldDef{
				{Key: "provider", Label: "服务商", Kind: KindText, Required: true},
				{Key: "region", Label: "区域", Kind: KindText},
				{Key: "spec", Label: "规格", Kind: KindText},
				{Key: "os", Label: "系统", Kind: KindText},
				{Key: "address", Label: "地址", Kind: KindText},
				{Key: "monthly_cost", Label: "月费", Kind: KindNumber, Unit: "元"},
				{Key: "expires_at", Label: "到期日", Kind: KindDate},
			}},
		{Key: "runbook", Label: "运维手册", Icon: wire.NavIconDoc, PrimaryField: "summary",
			Description: "一步步照着做的运维手册，关联到它适用的主机。",
			Statuses:    []string{"草稿", "可用"},
			Fields: []FieldDef{
				{Key: "summary", Label: "用途", Kind: KindText},
				{Key: "steps", Label: "步骤", Kind: KindText},
				{Key: "applies_to", Label: "适用主机", Kind: KindRelation, TargetType: "cloud_host", RelationType: "supports"},
				{Key: "url", Label: "参考链接", Kind: KindURL},
			}},
	}
}

// seeder creates the examples of a template, stopping at the first error.
type seeder struct {
	st  *State
	sp  *Space
	now time.Time
	err error
}

const seedActor = "模板"

func (s *seeder) add(typ, title, status string, props map[string]any, tags ...string) string {
	if s.err != nil {
		return ""
	}
	e, _, err := s.st.upsertEntity(EntityInput{SpaceID: s.sp.ID, Type: typ, Title: title, Status: status,
		Properties: props, Tags: tags}, seedActor, s.now)
	if err != nil {
		s.err = err
		return ""
	}
	return e.ID
}

func (s *seeder) relate(from, typ, to string) {
	if s.err != nil {
		return
	}
	_, s.err = s.st.addRelation(s.sp.ID, from, typ, to, seedActor, s.now)
}

func (s *seeder) incubate(idea string) string {
	if s.err != nil {
		return ""
	}
	e, err := s.st.incubate(idea, TypeGoal, seedActor, s.now)
	if err != nil {
		s.err = err
		return ""
	}
	return e.ID
}

func day(now time.Time, days int) string { return now.AddDate(0, 0, days).Format(time.DateOnly) }

func seedLifeOS(st *State, sp *Space, now time.Time) error {
	s := &seeder{st: st, sp: sp, now: now}
	fridge := s.add("location", "厨房冰箱", "", map[string]any{"room": "厨房", "note": "上层冷藏，下层冷冻"})
	cabinet := s.add("location", "阳台储物柜", "", map[string]any{"room": "阳台"})
	medbox := s.add("location", "客厅药箱", "", map[string]any{"room": "客厅", "note": "每季度检查一次有效期"})
	s.add("inventory_item", "纯牛奶", "快用完", map[string]any{"category": "食品", "location": fridge, "quantity": 2, "unit": "盒",
		"expiry_date": day(now, 5), "restock_needed": true}, "早餐")
	s.add("inventory_item", "鸡蛋", "在库", map[string]any{"category": "食品", "location": fridge, "quantity": 12, "unit": "个",
		"expiry_date": day(now, 14), "restock_needed": false})
	s.add("inventory_item", "洗衣液", "在库", map[string]any{"category": "清洁", "location": cabinet, "quantity": 1, "unit": "瓶",
		"restock_needed": false})
	s.add("inventory_item", "布洛芬", "在库", map[string]any{"category": "药品", "location": medbox, "quantity": 20, "unit": "片",
		"expiry_date": day(now, 300)})
	s.add("meal_plan", "周三晚餐", "计划中", map[string]any{"date": day(now, 1), "meal": "晚餐", "dishes": "番茄炒蛋、清炒时蔬",
		"ingredients": "鸡蛋 4 个、番茄 3 个、青菜 1 把", "servings": 3})
	s.add("meal_plan", "周末早午餐", "计划中", map[string]any{"date": day(now, 4), "meal": "午餐", "dishes": "牛奶燕麦、煎蛋三明治",
		"servings": 3})
	s.add("chore", "给绿植浇水", "待办", map[string]any{"assignee": "家人 A", "frequency": "每周", "due_date": day(now, 2),
		"area": "阳台", "minutes": 10})
	s.add("chore", "清理冰箱过期食品", "进行中", map[string]any{"assignee": "家人 B", "frequency": "每月", "due_date": day(now, 0),
		"area": "厨房", "minutes": 30})
	idea := s.add(TypeIdea, "周末批量备餐", IdeaExploring, map[string]any{"category": "饮食", "priority": "中",
		"summary": "周日下午做好三天的半成品，工作日晚上 15 分钟开饭"}, "备餐")
	s.add(TypeIdea, "给储物柜贴标签", IdeaInbox, map[string]any{"category": "收纳", "priority": "低",
		"summary": "按分类贴标签，找东西不用翻"})
	s.add(TypeGoal, "本季度囤货支出降低 20%", GoalActive, map[string]any{"target_value": 20, "current_value": 8, "unit": "%",
		"deadline": day(now, 60), "priority": "高", "milestone_summary": "先把重复购买的日用品清单列出来"})
	s.incubate(idea)
	s.add(TypeResource, "常用食材保质期速查", "using", map[string]any{"category": "清单", "url": "https://example.com/shelf-life",
		"summary": "冷藏、冷冻和常温各能放多久", "rating": 4.5})
	return s.err
}

func seedDeals(st *State, sp *Space, now time.Time) error {
	s := &seeder{st: st, sp: sp, now: now}
	jd := s.add("merchant", "某东自营数码", "常用", map[string]any{"platform": "电商 A", "channel_url": "https://example.com/shop/digital",
		"rating": 4.8, "note": "发货快，价保 30 天"})
	club := s.add("merchant", "会员仓储店小程序", "常用", map[string]any{"platform": "小程序", "rating": 4.6})
	coffee := s.add("merchant", "连锁咖啡", "观察", map[string]any{"platform": "外卖平台", "rating": 4.2})
	s.add("deal", "降噪耳机百亿补贴", "关注中", map[string]any{"platform": "电商 A", "merchant_name": "某东自营数码", "merchant": jd,
		"original_price": 1899, "deal_price": 1399, "saved_amount": 500, "valid_until": day(now, 3),
		"url": "https://example.com/deal/earbuds"}, "数码")
	s.add("deal", "坚果礼盒第二件半价", "已下单", map[string]any{"platform": "小程序", "merchant_name": "会员仓储店", "merchant": club,
		"original_price": 238, "deal_price": 179, "saved_amount": 59, "valid_until": day(now, 1)}, "零食")
	s.add("deal", "拿铁 9.9 元券包", "关注中", map[string]any{"platform": "外卖平台", "merchant_name": "连锁咖啡", "merchant": coffee,
		"original_price": 32, "deal_price": 9.9, "saved_amount": 22.1, "valid_until": day(now, 7)})
	s.add("coupon", "满 199 减 30 品类券", "可用", map[string]any{"platform": "电商 A", "threshold": 199, "discount": 30,
		"expires_at": day(now, 5), "code": "007", "merchant": jd})
	s.add("coupon", "饮品 8 折券", "已使用", map[string]any{"platform": "外卖平台", "discount": 6, "expires_at": day(now, -1), "merchant": coffee})
	idea := s.add(TypeIdea, "整理一份每月固定薅羊毛清单", IdeaInbox, map[string]any{"category": "习惯", "priority": "高",
		"summary": "每月 1 号、18 号固定检查会员日和信用卡活动"}, "清单")
	s.add(TypeIdea, "比价时记录历史最低价", IdeaExploring, map[string]any{"category": "工具", "priority": "中",
		"summary": "同一件商品多次出现时，自动对比到手价"})
	goal := s.add(TypeGoal, "本月省钱 500 元", GoalActive, map[string]any{"target_value": 500, "current_value": 186, "unit": "元",
		"deadline": day(now, 20), "priority": "高", "milestone_summary": "大件等百亿补贴，小件凑满减"})
	res := s.add(TypeResource, "信用卡优惠汇总表", "using", map[string]any{"category": "表格", "url": "https://example.com/card-offers",
		"summary": "各家银行的周末、会员日活动", "rating": 4})
	s.relate(res, "supports", goal)
	s.relate(idea, "supports", goal)
	return s.err
}

func seedCloud(st *State, sp *Space, now time.Time) error {
	s := &seeder{st: st, sp: sp, now: now}
	dev := s.add("cloud_host", "开发机 dev-01", "运行中", map[string]any{"provider": "示例云 A", "region": "华东",
		"spec": "4 核 8 GB", "os": "Ubuntu 24.04", "address": "<host-address>", "monthly_cost": 89, "expires_at": day(now, 25)}, "开发")
	s.add("cloud_host", "备份节点 bak-01", "已停止", map[string]any{"provider": "示例云 B", "region": "华南",
		"spec": "2 核 4 GB", "os": "Debian 12", "address": "<host-address>", "monthly_cost": 35, "expires_at": day(now, 90)}, "备份")
	s.add("runbook", "重装后初始化开发环境", "可用", map[string]any{"summary": "新机器到手后 20 分钟恢复工作环境",
		"steps": "1. 配置 SSH 密钥登录\n2. 安装 Go 与常用工具\n3. 拉取代码并跑一遍测试", "applies_to": dev,
		"url": "https://example.com/runbooks/dev-setup"})
	s.add("runbook", "到期前续费检查", "草稿", map[string]any{"summary": "到期前一周确认续费或迁移", "applies_to": dev})
	s.add(TypeIdea, "用快照代替手动备份", IdeaInbox, map[string]any{"category": "运维", "priority": "中",
		"summary": "每周自动快照，保留 4 份"})
	s.add(TypeGoal, "云支出控制在每月 150 元内", GoalActive, map[string]any{"target_value": 150, "current_value": 124, "unit": "元",
		"deadline": day(now, 30), "priority": "中", "milestone_summary": "停掉不用的节点"})
	return s.err
}

// seedState is the first state: the three example spaces, 薅羊毛空间 active (as Muse opens).
func seedState(now time.Time) *State {
	st := &State{Version: stateVersion}
	for _, key := range []string{"lifeos", "cloud", "deals"} {
		if _, err := st.createSpace(SpaceInput{Template: key}, seedActor, now); err != nil {
			panic("seed " + key + ": " + err.Error()) // the templates are ours: a test catches this
		}
	}
	// Order the sidebar as Muse does: 薅羊毛空间, LifeOS, 云电脑.
	order := map[string]int{"deals": 0, "lifeos": 1, "cloud": 2}
	sorted := make([]*Space, len(st.Spaces))
	for _, sp := range st.Spaces {
		sorted[order[sp.ID]] = sp
	}
	st.Spaces = sorted
	st.Active = "deals"
	return st
}
