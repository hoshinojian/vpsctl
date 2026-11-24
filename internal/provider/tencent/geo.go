package tencent

// geo.go：腾讯云地域 → 粗略坐标（等距投影用）。API 不返回坐标，只能静态映射；未知
// 地域缺省为 0，前端按"未定位"处理。云服务器与轻量应用服务器共用。
//
// 地域 ID 与城市的对应关系取自两家产品线 DescribeRegions 返回的地域名（2026-11-24
// 实测），不要凭印象增补——新增地域时先到该接口核对。

type geoPoint struct{ lat, lng float64 }

var regionGeo = map[string]geoPoint{
	// 中国大陆
	"ap-beijing":   {39.90, 116.41}, // 华北地区(北京)
	"ap-shanghai":  {31.23, 121.47}, // 华东地区(上海)
	"ap-nanjing":   {32.06, 118.80}, // 华东地区(南京)
	"ap-guangzhou": {23.13, 113.26}, // 华南地区(广州)
	"ap-chengdu":   {30.57, 104.07}, // 西南地区(成都)
	"ap-chongqing": {29.56, 106.55}, // 西南地区(重庆)
	// 港澳
	"ap-hongkong": {22.32, 114.17}, // 港澳台地区(中国香港)
	// 亚太
	"ap-singapore":  {1.35, 103.82},  // 东南亚地区(新加坡)；仅轻量应用服务器开放
	"ap-johorbahru": {1.49, 103.74},  // 亚太东南(新山)
	"ap-bangkok":    {13.76, 100.50}, // 亚太东南(曼谷)
	"ap-jakarta":    {-6.21, 106.85}, // 亚太东南(雅加达)
	"ap-seoul":      {37.57, 126.98}, // 亚太东北(首尔)
	"ap-tokyo":      {35.68, 139.69}, // 亚太东北(东京)
	"ap-osaka":      {34.69, 135.50}, // 亚太东北(大阪)
	// 中东与欧洲
	"me-saudi-arabia": {24.71, 46.68}, // 中东地区(利雅得)
	"eu-frankfurt":    {50.11, 8.68},  // 欧洲地区(法兰克福)
	// 美洲
	"na-queretaro":     {20.59, -100.39}, // 北美地区(克雷塔罗)
	"na-siliconvalley": {37.77, -122.42}, // 美国西部(硅谷)
	"na-ashburn":       {39.04, -77.49},  // 美国东部(弗吉尼亚)
	"sa-saopaulo":      {-23.55, -46.63}, // 南美地区(圣保罗)
}

// RegionGeo 返回地域的粗略坐标；未知地域返回 false。
func RegionGeo(region string) (lat, lng float64, ok bool) {
	g, ok := regionGeo[region]
	return g.lat, g.lng, ok
}
