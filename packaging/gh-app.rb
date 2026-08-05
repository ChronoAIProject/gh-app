class GhApp < Formula
  desc "Repository-scoped GitHub App credential resolver"
  homepage "https://github.com/ChronoAIProject/gh-app"

  # Release preparation: replace both placeholders after the tag is pushed.
  url "https://github.com/ChronoAIProject/gh-app/archive/refs/tags/vVERSION_PLACEHOLDER.tar.gz"
  sha256 "SHA256_PLACEHOLDER"

  depends_on "go" => :build

  def install
    system "make", "VERSION=v#{version}", "build"
    bin.install "gh-app"
  end

  test do
    assert_equal "v#{version}\n", shell_output("#{bin}/gh-app version")
  end
end
