// Package pairing derives the words a super user compares when confirming
// a new Pi (DESIGN.md §6.3, decision D15): the same six words appear on the
// Pi and on central's pending-devices page, derived from the Pi's keys.
package pairing

import (
	"crypto/sha256"
	"strings"
)

// Words returns six words (48 bits) derived from the SHA-256 of the given
// keys. The keys are generated on the employee's Pi, so someone activating
// a different Pi with stolen credentials cannot know which words to match.
func Words(keys ...[]byte) string {
	h := sha256.New()
	for _, k := range keys {
		h.Write(k)
	}
	sum := h.Sum(nil)
	out := make([]string, 6)
	for i := range out {
		out[i] = wordlist[sum[i]]
	}
	return strings.Join(out, " ")
}

var wordlist = [256]string{
	"acorn", "amber", "anchor", "apple", "arrow", "aspen", "atlas", "autumn",
	"badge", "bagel", "bamboo", "banjo", "barley", "basil", "beacon", "beaver",
	"berry", "birch", "bison", "blaze", "bloom", "bluff", "bonsai", "border",
	"bramble", "breeze", "brick", "bridge", "brook", "bubble", "bucket", "bugle",
	"cabin", "cactus", "camel", "candle", "canoe", "canyon", "carbon", "cargo",
	"carrot", "castle", "cedar", "cello", "chalk", "cherry", "chess", "cider",
	"cinder", "circus", "citrus", "clover", "cobalt", "cocoa", "comet", "copper",
	"coral", "cotton", "coyote", "crane", "crater", "cricket", "crystal", "cypress",
	"daisy", "dancer", "delta", "denim", "desert", "dingo", "dolphin", "domino",
	"donkey", "dragon", "drum", "dune", "eagle", "easel", "ebony", "echo",
	"elbow", "elm", "ember", "emerald", "engine", "falcon", "fennel", "fern",
	"ferry", "fiddle", "fig", "finch", "fjord", "flame", "flint", "forest",
	"fossil", "fountain", "fox", "galaxy", "garden", "garnet", "gecko", "geyser",
	"ginger", "glacier", "globe", "goose", "granite", "grape", "gravel", "guitar",
	"hammer", "harbor", "harp", "hazel", "heron", "hickory", "honey", "horizon",
	"hornet", "husky", "iceberg", "igloo", "indigo", "iris", "island", "ivory",
	"jade", "jaguar", "jasmine", "jelly", "jigsaw", "juniper", "kayak", "kernel",
	"kettle", "kiwi", "koala", "ladder", "lagoon", "lantern", "larch", "lava",
	"lemon", "lentil", "lily", "lime", "linen", "lizard", "llama", "lobster",
	"locket", "lotus", "lumber", "lynx", "magnet", "mango", "maple", "marble",
	"meadow", "melon", "meteor", "mint", "mitten", "mohair", "moose", "mosaic",
	"moss", "muffin", "nectar", "needle", "nickel", "nutmeg", "oasis", "oat",
	"ocean", "olive", "onyx", "orbit", "orchid", "otter", "oyster", "paddle",
	"panda", "papaya", "parrot", "pebble", "pelican", "pepper", "piano", "pine",
	"pistachio", "planet", "plum", "pollen", "poppy", "prairie", "prism", "puffin",
	"pumpkin", "quartz", "quill", "quince", "rabbit", "radish", "raven", "reef",
	"ribbon", "river", "robin", "rocket", "saddle", "saffron", "salmon", "sandal",
	"sapphire", "satchel", "sequoia", "shadow", "shell", "sierra", "silver", "sparrow",
	"spruce", "squid", "summit", "sunset", "swan", "tandem", "tango", "teapot",
	"thistle", "thunder", "tiger", "timber", "topaz", "tulip", "tundra", "turtle",
	"umbrella", "valley", "velvet", "violet", "walnut", "walrus", "willow", "yarrow",
	"zebra", "zephyr", "zinnia", "badger", "cobble", "meadowlark", "spindle", "wren",
}
