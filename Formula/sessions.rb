class Sessions < Formula
  desc "Durable local Claude Code, Codex, and terminal sessions"
  homepage "https://sessions.somewhere.tech"
  version "0.1.0"
  license "MIT"

  on_macos do
    depends_on arch: :arm64
    on_arm do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.1.0/sessions_0.1.0_darwin_arm64.tar.gz"
      sha256 "a3f2327cd0f4e27d72849334df785acbe923e74771a911c110528a208d4ee041"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.1.0/sessions_0.1.0_linux_arm64.tar.gz"
      sha256 "275c2fae5c6c84562b1f8fe40912d3e5f5c5888085cfc11fcdf20da1a41c714d"
    end

    on_intel do
      url "https://github.com/somewhere-tech/sessions/releases/download/v0.1.0/sessions_0.1.0_linux_amd64.tar.gz"
      sha256 "7e41d380a607dbb8884c561b283bfd98aac4c12a0629d92a6ea73438bfe28c09"
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

        Then open http://localhost:8787.
      EOS
    end
  end

  test do
    system bin/"sessions", "help"
  end
end
