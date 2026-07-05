package main

import "strings"

// GEOSITE categories bundled for one-click GFW-style routing. These are
// curated, commonly-hit subsets (not the exhaustive GFWList / china-list) -
// enough to make "domestic direct, blocked via proxy" work out of the box,
// and users can still add their own DOMAIN-SUFFIX rules for anything else.
const (
	GeositeCN  = "cn"  // mainland-China sites that should bypass the proxy
	GeositeGFW = "gfw" // well-known GFW-blocked sites that need the proxy
)

// geositeCN: mainland China domains (matched as suffixes). ".cn" alone
// catches most, these cover the big non-.cn properties.
var geositeCN = []string{
	"cn",
	"baidu.com", "qq.com", "weixin.qq.com", "taobao.com", "tmall.com",
	"jd.com", "alipay.com", "aliyun.com", "aliyuncs.com", "alicdn.com",
	"1688.com", "taobaocdn.com", "tanx.com", "mmstat.com",
	"bilibili.com", "hdslb.com", "bilivideo.com", "acgvideo.com",
	"weibo.com", "weibo.cn", "sinaimg.cn", "sina.com.cn", "sina.com",
	"zhihu.com", "zhimg.com", "163.com", "126.com", "netease.com",
	"126.net", "127.net", "music.163.com", "sohu.com", "sohucs.com",
	"iqiyi.com", "qiyi.com", "youku.com", "ykimg.com", "tudou.com",
	"douyin.com", "douyinvod.com", "toutiao.com", "byteimg.com",
	"snssdk.com", "pstatp.com", "ixigua.com", "feishu.cn", "feishu.net",
	"meituan.com", "meituan.net", "dianping.com", "sankuai.com",
	"ele.me", "eleme.cn", "didiglobal.com", "xiaojukeji.com",
	"xiaohongshu.com", "xhscdn.com", "kuaishou.com", "kwimgs.com",
	"pinduoduo.com", "yangkeduo.com", "pddpic.com",
	"douban.com", "doubanio.com", "ctrip.com", "tripcdn.cn",
	"58.com", "ganji.com", "anjuke.com", "lianjia.com", "beike.cn",
	"12306.cn", "gov.cn", "edu.cn", "org.cn", "net.cn", "com.cn",
	"csdn.net", "csdnimg.cn", "cnblogs.com", "gitee.com", "coding.net",
	"jianshu.com", "jianshu.io", "segmentfault.com", "juejin.cn",
	"huawei.com", "hicloud.com", "vmall.com", "honor.com",
	"mi.com", "xiaomi.com", "miui.com", "micdn.com", "duokan.com",
	"oppo.com", "vivo.com.cn", "oneplus.com",
	"tencent.com", "gtimg.cn", "qpic.cn", "qlogo.cn", "qcloud.com",
	"myqcloud.com", "tencent-cloud.net", "wechat.com",
	"kugou.com", "kuwo.cn", "qqmusic.qq.com", "ximalaya.com",
	"360.cn", "360.com", "qhimg.com", "qhres.com", "so.com",
	"ucweb.com", "uc.cn", "quark.cn", "shifen.com", "bdstatic.com",
	"bdimg.com", "baidubce.com", "bcebos.com", "baidupcs.com",
	"amap.com", "autonavi.com", "gaode.com", "baidumap.com",
}

// geositeGFW: sites commonly blocked by the GFW that need the proxy.
var geositeGFW = []string{
	"google.com", "google.com.hk", "googleapis.com", "gstatic.com",
	"googleusercontent.com", "googlevideo.com", "ggpht.com", "goo.gl",
	"withgoogle.com", "google-analytics.com", "youtube.com", "youtu.be",
	"ytimg.com", "youtubei.googleapis.com",
	"facebook.com", "fb.com", "fbcdn.net", "fbsbx.com", "messenger.com",
	"instagram.com", "cdninstagram.com", "whatsapp.com", "whatsapp.net",
	"twitter.com", "x.com", "twimg.com", "t.co", "twitter.co",
	"telegram.org", "telegram.me", "t.me", "telesco.pe", "tdesktop.com",
	"telegra.ph",
	"wikipedia.org", "wikimedia.org", "wikidata.org", "wiktionary.org",
	"reddit.com", "redd.it", "redditstatic.com", "redditmedia.com",
	"discord.com", "discord.gg", "discordapp.com", "discordapp.net",
	"twitch.tv", "ttvnw.net", "jtvnw.net",
	"pornhub.com", "phncdn.com",
	"tumblr.com", "medium.com", "quora.com", "pinterest.com",
	"blogspot.com", "blogger.com", "wordpress.org",
	"nytimes.com", "bbc.com", "bbc.co.uk", "theguardian.com",
	"wsj.com", "reuters.com", "bloomberg.com", "cnn.com",
	"onion", "torproject.org",
	"spotify.com", "scdn.co", "spotifycdn.com",
	"netflix.com", "nflxvideo.net", "nflximg.net", "nflxext.com",
	"disneyplus.com", "hulu.com", "hbomax.com", "hbo.com",
	"github.com", "githubusercontent.com", "githubassets.com", "ghcr.io",
	"openai.com", "chatgpt.com", "oaistatic.com", "oaiusercontent.com",
	"anthropic.com", "claude.ai", "cloudflare.com", "gemini.google.com",
	"line.me", "line-scdn.net", "kakao.com", "naver.com",
	"dropbox.com", "dropboxusercontent.com", "onedrive.com",
	"protonmail.com", "proton.me", "signal.org",
}

var (
	geositeCNSet  = buildSuffixSet(geositeCN)
	geositeGFWSet = buildSuffixSet(geositeGFW)
)

func buildSuffixSet(list []string) map[string]bool {
	m := make(map[string]bool, len(list))
	for _, d := range list {
		m[strings.ToLower(d)] = true
	}
	return m
}

// geositeMatch reports whether host belongs to a bundled category, using
// domain-suffix semantics (a.b.example.com matches example.com).
func geositeMatch(category, host string) bool {
	var set map[string]bool
	switch category {
	case GeositeCN:
		set = geositeCNSet
	case GeositeGFW:
		set = geositeGFWSet
	default:
		return false
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for {
		if set[host] {
			return true
		}
		i := strings.IndexByte(host, '.')
		if i < 0 {
			return false
		}
		host = host[i+1:]
	}
}

func validGeositeCategory(c string) bool {
	return c == GeositeCN || c == GeositeGFW
}
