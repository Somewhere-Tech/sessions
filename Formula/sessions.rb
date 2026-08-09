class Sessions < Formula
  desc "Durable local Claude Code, Codex, and terminal sessions"
  homepage "https://sessions.somewhere.tech"
  version "0.2.18"
  license "MIT"

  on_macos do
    depends_on arch: :arm64
    on_arm do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.2.18/sessions_0.2.18_darwin_arm64.tar.gz"
      sha256 "a10b5a9d4fd4fd0e627a8859987fabec652ea25e15fd37f10f9d82e84dc6688f"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.2.18/sessions_0.2.18_linux_arm64.tar.gz"
      sha256 "fa4f1127e35ba3ca3f21e87582ab0e1cd3900771e4027818efa2d77113f52e9a"
    end

    on_intel do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.2.18/sessions_0.2.18_linux_amd64.tar.gz"
      sha256 "14ab0e180668e131c560191b0834d95fd284ebb6b40beb6fb9748859754fc5ae"
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
