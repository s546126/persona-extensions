package imports

import "strings"

// guess is what the built-in table suggests for a kind of thing: its category, words that a fitting place's title
// contains (the first place of the household that has one of them is suggested), and how many days it keeps once
// bought, 0 when there is no sensible default. Every value it gives is marked 推测 and can be changed.
type guess struct {
	words    []string // any of these in the name picks the entry
	category string
	places   []string
	days     int
}

// Place words, from the most to the least specific.
var (
	cold   = []string{"冷藏", "冰箱"}
	frozen = []string{"冷冻", "冰柜", "冰箱"}
	dry    = []string{"厨房柜", "橱柜", "储物", "厨房"}
	bath   = []string{"卫生间", "浴室", "洗手间"}
	store  = []string{"杂物", "储物", "柜"}
	meds   = []string{"药箱", "药", "杂物"}
)

// table is the built-in list of common groceries and household goods (see lookup for how a name finds its entry). Shelf lives are the usual ones for a fresh, unopened
// product kept in the suggested place; things whose shelf life depends too much on the product (牛奶 in general, rice,
// sauces, cleaning products) have none.
var table = []guess{
	{words: []string{"鲜牛奶", "鲜奶", "巴氏奶"}, category: "食材", places: cold, days: 7},
	{words: []string{"酸奶", "酸牛奶"}, category: "食材", places: cold, days: 14},
	{words: []string{"牛奶", "豆奶", "奶粉"}, category: "食材", places: dry},
	{words: []string{"鸡蛋", "鸭蛋", "鹌鹑蛋"}, category: "食材", places: cold, days: 30},
	{words: []string{"豆腐", "豆芽"}, category: "食材", places: cold, days: 3},
	{words: []string{"面包", "吐司", "馒头", "包子"}, category: "食材", places: dry, days: 5},
	{words: []string{"生菜", "菠菜", "青菜", "油菜", "小白菜", "韭菜", "香菜", "葱"}, category: "食材", places: cold, days: 3},
	{words: []string{"番茄", "西红柿", "黄瓜", "茄子", "青椒", "辣椒", "西兰花", "白菜"}, category: "食材", places: cold, days: 7},
	{words: []string{"土豆", "洋葱", "红薯", "南瓜", "大蒜", "蒜", "姜"}, category: "食材", places: dry, days: 30},
	{words: []string{"胡萝卜", "萝卜"}, category: "食材", places: cold, days: 14},
	{words: []string{"香蕉", "草莓", "葡萄"}, category: "食材", places: dry, days: 5},
	{words: []string{"苹果", "梨", "橙", "橘", "柚"}, category: "食材", places: dry, days: 21},
	{words: []string{"水饺", "汤圆", "馄饨", "冻肉"}, category: "食材", places: frozen},
	{words: []string{"猪肉", "牛肉", "羊肉", "鸡肉", "鸡腿", "鸡翅", "鸡胸", "排骨", "肉末", "五花肉"}, category: "食材", places: cold, days: 2},
	{words: []string{"鱼", "虾", "蟹", "贝"}, category: "食材", places: cold, days: 1},
	{words: []string{"酱油", "醋", "盐", "糖", "料酒", "蚝油", "味精", "鸡精", "花椒", "八角", "辣酱", "豆瓣酱", "调料", "香油"}, category: "调味", places: dry},
	{words: []string{"食用油", "花生油", "菜籽油", "橄榄油", "玉米油", "调和油"}, category: "调味", places: dry},
	{words: []string{"米", "面粉", "面条", "挂面", "燕麦"}, category: "食材", places: dry},
	{words: []string{"婴儿湿巾", "纸尿裤", "尿不湿", "尿片", "宝宝", "婴儿"}, category: "母婴", places: store},
	{words: []string{"洗衣液", "洗衣粉", "洗洁精", "消毒", "洁厕", "漂白", "清洁剂", "洗衣凝珠"}, category: "清洁", places: bath},
	{words: []string{"卫生纸", "抽纸", "纸巾", "湿巾", "厨房纸", "垃圾袋", "保鲜膜", "保鲜袋", "电池"}, category: "日用", places: store},
	{words: []string{"牙膏", "牙刷", "洗发", "沐浴", "香皂", "肥皂", "洗手液", "护发素"}, category: "日用", places: bath},
	{words: []string{"感冒", "退烧", "布洛芬", "创可贴", "维生素", "药", "碘伏", "口罩"}, category: "药品", places: meds},
}

// notProducts are names that end in a word of the table but are another product: a suggestion for them would be
// wrong, and no suggestion is better than a wrong one.
var notProducts = []string{"棉花糖", "奶糖", "软糖", "口香糖", "水果糖", "宝贝", "果冻", "酒心", "米酒", "米粉", "米线", "葱油",
	"凤梨", "姜糖", "农药", "老鼠药", "鼠药", "杀虫", "灭蚊", "兽药"}

// lookup is the table entry for a name, false when none fits. A word must be the whole name or its end (the head of a
// Chinese product name: 土鸡蛋, 进口香蕉), after sizes and brackets are left out, so that 鱼罐头,
// 贝果, 葱油饼, 糖果, 盐焗鸡 and 果冻 match nothing. The longest word wins (洋葱 over 葱); names on notProducts match
// nothing.
func lookup(title string) (guess, bool) {
	k := strings.ToLower(halfWidth(title))
	k = titleKey(sizePattern.ReplaceAllString(bracketPattern.ReplaceAllString(k, ""), ""))
	if k == "" {
		return guess{}, false
	}
	for _, x := range notProducts {
		if strings.Contains(k, x) {
			return guess{}, false
		}
	}
	best, bestLen := guess{}, 0
	for _, g := range table {
		for _, w := range g.words {
			if n := len([]rune(w)); strings.HasSuffix(k, w) && n > bestLen {
				best, bestLen = g, n
			}
		}
	}
	return best, bestLen > 0
}
