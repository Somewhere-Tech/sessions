class Sessions < Formula
  desc "Durable local Claude Code, Codex, and terminal sessions"
  homepage "https://sessions.somewhere.tech"
  version "0.2.26"
  license "MIT"

  on_macos do
    depends_on arch: :arm64
    on_arm do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.2.26/sessions_0.2.26_darwin_arm64.tar.gz"
      sha256 "83d2137d4b94bdd0d43df8de5d46afa49d4d13c52f4e5cec9bafe876ee68ed52"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.2.26/sessions_0.2.26_linux_arm64.tar.gz"
      sha256 "cf9112b9e1386dcc51acbd80eac6b410d1204f390c6ac21998428222574ad4fd"
    end

    on_intel do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.2.26/sessions_0.2.26_linux_amd64.tar.gz"
      sha256 "1ad51e499c54cdbd4ffc6da0afa1f2db27f49013283b9ad04502f12b07b0ded9"
    end
  end

  def install
    bin.install "sessions", "sessionsd", "sessions-runner"
  end

  def caveats
    on_macos do
      <<~EOS
        Register and start the per-user daemon with:
          sessions install

        Verify the local service with:
          sessions status
      EOS
    end
  end

  test do
    system bin/"sessions", "help"
  end
end
