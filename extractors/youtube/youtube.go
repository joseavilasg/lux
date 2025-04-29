package youtube

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/kkdai/youtube/v2"
	"github.com/pkg/errors"

	"github.com/iawia002/lux/extractors"
	"github.com/iawia002/lux/request"
	"github.com/iawia002/lux/utils"
)

func init() {
	e := New()
	extractors.Register("youtube", e)
	extractors.Register("youtu", e) // youtu.be
}

const referer = "https://www.youtube.com"

type extractor struct {
	client *youtube.Client
}

type youtubeTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *youtubeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	return t.base.RoundTrip(req)
}

func getVisitorId() (string, error) {
	const sep = "\nytcfg.set("
	req, err := http.NewRequest("GET", "https://www.youtube.com", nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to perform request: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	_, ytcnf, found := strings.Cut(string(data), sep)
	if !found {
		return "", fmt.Errorf("separator not found in response")
	}

	var value struct {
		InnertubeContext struct {
			Client struct {
				VisitorData string `json:"visitorData"`
			} `json:"client"`
		} `json:"INNERTUBE_CONTEXT"`
	}

	if err := json.NewDecoder(strings.NewReader(ytcnf)).Decode(&value); err != nil {
		return "", fmt.Errorf("failed to decode JSON: %w", err)
	}

	visitor, err := url.PathUnescape(value.InnertubeContext.Client.VisitorData)
	if err != nil {
		return "", fmt.Errorf("failed to unescape visitor data: %w", err)
	}

	return visitor, nil
}

// New returns a youtube extractor.
func New() extractors.Extractor {
	visitorId, err := getVisitorId()
	if err != nil {
		panic(fmt.Sprintf("failed to get visitorId: %v", err))
	}

	return &extractor{
		client: &youtube.Client{
			HTTPClient: &http.Client{
				Transport: &youtubeTransport{
					base: &http.Transport{
						Proxy: http.ProxyFromEnvironment,
					},
					headers: map[string]string{
						"x-goog-visitor-id": visitorId,
					},
				},
			},
		},
	}
}

// Extract is the main function to extract the data.
func (e *extractor) Extract(url string, option extractors.Options) ([]*extractors.Data, error) {
	if !option.Playlist {
		video, err := e.client.GetVideo(url)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		return []*extractors.Data{e.youtubeDownload(url, video)}, nil
	}

	playlist, err := e.client.GetPlaylist(url)
	if err != nil {
		return nil, errors.WithStack(err)
	}

	needDownloadItems := utils.NeedDownloadList(option.Items, option.ItemStart, option.ItemEnd, len(playlist.Videos))
	extractedData := make([]*extractors.Data, len(needDownloadItems))
	wgp := utils.NewWaitGroupPool(option.ThreadNumber)
	dataIndex := 0
	for index, videoEntry := range playlist.Videos {
		if !slices.Contains(needDownloadItems, index+1) {
			continue
		}

		wgp.Add()
		go func(index int, entry *youtube.PlaylistEntry, extractedData []*extractors.Data) {
			defer wgp.Done()
			video, err := e.client.VideoFromPlaylistEntry(entry)
			if err != nil {
				return
			}
			extractedData[index] = e.youtubeDownload(url, video)
		}(dataIndex, videoEntry, extractedData)
		dataIndex++
	}
	wgp.Wait()
	return extractedData, nil
}

// youtubeDownload download function for single url
func (e *extractor) youtubeDownload(url string, video *youtube.Video) *extractors.Data {
	streams := make(map[string]*extractors.Stream, len(video.Formats))
	audioCache := make(map[string]*extractors.Part)

	for i := range video.Formats {
		f := &video.Formats[i]
		itag := strconv.Itoa(f.ItagNo)
		quality := f.MimeType
		if f.QualityLabel != "" {
			quality = fmt.Sprintf("%s %s", f.QualityLabel, f.MimeType)
		}

		part, err := e.genPartByFormat(video, f)
		if err != nil {
			return extractors.EmptyData(url, err)
		}
		stream := &extractors.Stream{
			ID:      itag,
			Parts:   []*extractors.Part{part},
			Quality: quality,
			Ext:     part.Ext,
			NeedMux: true,
		}

		// Unlike `url_encoded_fmt_stream_map`, all videos in `adaptive_fmts` have no sound,
		// we need download video and audio both and then merge them.
		// video format with audio:
		//   AudioSampleRate: "44100", AudioChannels: 2
		// video format without audio:
		//   AudioSampleRate: "", AudioChannels: 0
		if f.AudioChannels == 0 {
			audioPart, ok := audioCache[part.Ext]
			if !ok {
				audio, err := getVideoAudio(video, part.Ext)
				if err != nil {
					return extractors.EmptyData(url, err)
				}
				audioPart, err = e.genPartByFormat(video, audio)
				if err != nil {
					return extractors.EmptyData(url, err)
				}
				audioCache[part.Ext] = audioPart
			}
			stream.Parts = append(stream.Parts, audioPart)
		}
		streams[itag] = stream
	}

	return &extractors.Data{
		Site:    "YouTube youtube.com",
		Title:   video.Title,
		Type:    "video",
		Streams: streams,
		URL:     url,
	}
}

func (e *extractor) genPartByFormat(video *youtube.Video, f *youtube.Format) (*extractors.Part, error) {
	ext := getStreamExt(f.MimeType)
	url, err := e.client.GetStreamURL(video, f)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	size := f.ContentLength
	if size == 0 {
		size, _ = request.Size(url, referer)
	}
	return &extractors.Part{
		URL:  url,
		Size: size,
		Ext:  ext,
	}, nil
}

func getVideoAudio(v *youtube.Video, mimeType string) (*youtube.Format, error) {
	audioFormats := v.Formats.Type(mimeType).Type("audio")
	if len(audioFormats) == 0 {
		return nil, errors.New("no audio format found after filtering")
	}
	audioFormats.Sort()
	return &audioFormats[0], nil
}

func getStreamExt(streamType string) string {
	// video/webm; codecs="vp8.0, vorbis" --> webm
	exts := utils.MatchOneOf(streamType, `(\w+)/(\w+);`)
	if exts == nil || len(exts) < 3 {
		return ""
	}
	return exts[2]
}
