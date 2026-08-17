package api

import (
	"encoding/json"
	"strconv"
	"time"
)

// PhotoMetadata is the normalized, cacheable record of one photo's download
// URL and file characteristics (ADR-019/021). It lets an incomplete
// photoset be repaired — or a verify pass check file integrity — without
// re-reading the photoset's listing pages.
type PhotoMetadata struct {
	ID             string
	URL            string
	Media          string
	OriginalFormat string
	Extension      string
	OWidth         int
	OHeight        int
	SizeBytes      int64
	UpdatedAt      time.Time
}

type FlexInt int

func (f *FlexInt) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s == "" {
			*f = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*f = FlexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = FlexInt(n)
	return nil
}

type Photo struct {
	ID             string  `json:"id"`
	Secret         string  `json:"secret"`
	Server         string  `json:"server"`
	Farm           FlexInt `json:"farm"`
	Title          string  `json:"title"`
	IsPublic       FlexInt `json:"ispublic"`
	Owner          string  `json:"owner"`
	OriginalSecret string  `json:"originalsecret"`
	OriginalFormat string  `json:"originalformat"`
	URLOriginal    string  `json:"url_o"`
	Media          string  `json:"media"`
	OWidth         FlexInt `json:"o_width"`
	OHeight        FlexInt `json:"o_height"`
}

type PhotosResponse struct {
	Photos struct {
		Page    FlexInt `json:"page"`
		Pages   FlexInt `json:"pages"`
		PerPage FlexInt `json:"perpage"`
		Total   FlexInt `json:"total"`
		Photo   []Photo `json:"photo"`
	} `json:"photos"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type PhotoSetResponse struct {
	Photoset struct {
		ID      string  `json:"id"`
		Owner   string  `json:"owner"`
		Primary string  `json:"primary"`
		Page    FlexInt `json:"page"`
		Pages   FlexInt `json:"pages"`
		PerPage FlexInt `json:"perpage"`
		Total   FlexInt `json:"total"`
		Photo   []Photo `json:"photo"`
	} `json:"photoset"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type PhotoSize struct {
	Label  string  `json:"label"`
	Width  FlexInt `json:"width"`
	Height FlexInt `json:"height"`
	Source string  `json:"source"`
	URL    string  `json:"url"`
	Media  string  `json:"media"`
}

type SizesResponse struct {
	Sizes struct {
		CanBlog     FlexInt     `json:"canblog"`
		CanPrint    FlexInt     `json:"canprint"`
		CanDownload FlexInt     `json:"candownload"`
		Size        []PhotoSize `json:"size"`
	} `json:"sizes"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type PhotoSetInfo struct {
	ID    string `json:"id"`
	Owner string `json:"owner"`
	Title struct {
		Content string `json:"_content"`
	} `json:"title"`
	Description struct {
		Content string `json:"_content"`
	} `json:"description"`
	Photos    FlexInt `json:"photos"`
	UpdatedAt FlexInt `json:"date_update,omitempty"`
}

type PhotoSetsListResponse struct {
	Photosets struct {
		Page     FlexInt        `json:"page"`
		Pages    FlexInt        `json:"pages"`
		PerPage  FlexInt        `json:"perpage"`
		Total    FlexInt        `json:"total"`
		Photoset []PhotoSetInfo `json:"photoset"`
	} `json:"photosets"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type PhotoSetInfoResponse struct {
	Photoset PhotoSetInfo `json:"photoset"`
	Stat     string       `json:"stat"`
	Code     int          `json:"code"`
	Message  string       `json:"message"`
}

type PhotoInfoResponse struct {
	Photo struct {
		ID     string  `json:"id"`
		Secret string  `json:"secret"`
		Server string  `json:"server"`
		Farm   FlexInt `json:"farm"`
		Owner  struct {
			NSID     string `json:"nsid"`
			Username string `json:"username"`
		} `json:"owner"`
		Title struct {
			Content string `json:"_content"`
		} `json:"title"`
		OriginalSecret string `json:"originalsecret"`
		OriginalFormat string `json:"originalformat"`
		Media          string `json:"media"`
	} `json:"photo"`
	Stat    string `json:"stat"`
	Code    int    `json:"code"`
	Message string `json:"message"`
}
