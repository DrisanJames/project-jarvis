package api

import (
	"os"
	"strings"
)

// BrandKit holds brand identity and API configuration for a web property.
type BrandKit struct {
	Key            string
	SiteDomain     string
	SiteName       string
	Tagline        string
	PrimaryColor   string
	SecondaryColor string
	AccentColor    string
	HeadingFont    string
	BodyFont       string
	Voice          string
	LogoHTML       string
	SiteAPIURL     string
	SiteAPIKey     string
	SendingDomain  string
	ImageDomain    string
}

var brandKits = map[string]BrandKit{
	"discountblog": {
		Key:            "discountblog",
		SiteDomain:     "discountblog.com",
		SiteName:       "DiscountBlog",
		Tagline:        "Smart Savings for Busy Families",
		PrimaryColor:   "#FF6B6B",
		SecondaryColor: "#2EC4B6",
		AccentColor:    "#FFD166",
		HeadingFont:    "Playfair Display",
		BodyFont:       "Inter",
		Voice:          "Friendly, practical, budget-conscious. Write as 'Jamie' - a savvy deal-finder who helps families save.",
		LogoHTML:       `<span style="color:#FF7B7B;">discount</span><span style="color:#5FCCB8;">blog</span>`,
		SiteAPIURL:     "https://discountblog.com/api/reviews",
		SiteAPIKey:     "",
		SendingDomain:  "em.discountblog.com",
		ImageDomain:    "img.discountblog.com",
	},
	"quizfiesta": {
		Key:            "quizfiesta",
		SiteDomain:     "quizfiesta.com",
		SiteName:       "QuizFiesta",
		Tagline:        "Play, Learn, Win!",
		PrimaryColor:   "#7C3AED",
		SecondaryColor: "#EC4899",
		AccentColor:    "#F59E0B",
		HeadingFont:    "Fredoka One",
		BodyFont:       "Nunito",
		Voice:          "Fun, energetic, youthful. Think game show host meets savvy consumer.",
		LogoHTML:       `<span style="color:#7C3AED;">Quiz</span><span style="color:#EC4899;">Fiesta</span>`,
		SiteAPIURL:     "https://quizfiesta.com/api/reviews",
		SiteAPIKey:     "",
		SendingDomain:  "em.quizfiesta.com",
		ImageDomain:    "img.quizfiesta.com",
	},
	"historythinking": {
		Key:            "historythinking",
		SiteDomain:     "historythinking.com",
		SiteName:       "History Thinking",
		Tagline:        "Understanding the Past, Shaping the Future",
		PrimaryColor:   "#8B4513",
		SecondaryColor: "#DAA520",
		AccentColor:    "#2F4F4F",
		HeadingFont:    "Playfair Display",
		BodyFont:       "Source Serif 4",
		Voice:          "Scholarly but accessible. Write as a knowledgeable historian who connects past to present.",
		LogoHTML:       `<span style="font-family:'Playfair Display';color:#8B4513;">History</span><span style="color:#DAA520;">Thinking</span>`,
		SiteAPIURL:     "https://historythinking.com/api/reviews",
		SiteAPIKey:     "",
		SendingDomain:  "em.historythinking.com",
		ImageDomain:    "img.historythinking.com",
	},
	"myownhealth": {
		Key:            "myownhealth",
		SiteDomain:     "myownhealth.net",
		SiteName:       "My Own Health",
		Tagline:        "Your Health, Your Way",
		PrimaryColor:   "#10B981",
		SecondaryColor: "#3B82F6",
		AccentColor:    "#F97316",
		HeadingFont:    "Poppins",
		BodyFont:       "Open Sans",
		Voice:          "Warm, evidence-based, encouraging. Write as a wellness coach who respects readers' intelligence.",
		LogoHTML:       `<span style="color:#10B981;">My Own</span><span style="color:#3B82F6;">Health</span>`,
		SiteAPIURL:     "https://myownhealth.net/api/reviews",
		SiteAPIKey:     "",
		SendingDomain:  "em.myownhealth.net",
		ImageDomain:    "img.myownhealth.net",
	},
}

// GetBrandKit returns the BrandKit for the given web property key.
func GetBrandKit(webProperty string) (BrandKit, bool) {
	kit, ok := brandKits[webProperty]
	return kit, ok
}

// GetBrandKitBySendingDomain returns the BrandKit whose SendingDomain matches.
func GetBrandKitBySendingDomain(domain string) (BrandKit, bool) {
	for _, kit := range brandKits {
		if kit.SendingDomain == domain {
			return kit, true
		}
	}
	return BrandKit{}, false
}

func init() {
	for key, kit := range brandKits {
		envKey := strings.ToUpper(strings.ReplaceAll(key, "-", "_")) + "_API_KEY"
		if v := os.Getenv(envKey); v != "" {
			kit.SiteAPIKey = v
			brandKits[key] = kit
		}
	}
}
