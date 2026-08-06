; Sessions installs two things outside its own program directory: the per-user
; logon supervisor definition that brings the daemon back at every sign-in, and
; one managed entry in the per-user PATH. Removing the program files leaves both
; behind, so uninstall has to take them out explicitly.
;
; It must not take out anything else. Session records, the ledger, the saved
; port, paired-machine credentials, and the staged runtime under
; %LOCALAPPDATA% are the user's work, or the exact bytes a live daemon and its
; runners are executing right now. Nothing here stops a runner or a daemon
; either: uninstalling the viewer is not a reason to end an agent that is still
; working, and the supervisor simply does not return once its definition is
; gone.

!macro NSIS_HOOK_PREUNINSTALL
  ; PATH is a value the user also edits by hand and shares with unrelated tools.
  ; Rewriting it with string surgery here would be an unreviewable way to damage
  ; someone's environment, so hand it to the same Rust code that wrote the entry
  ; (windows_cli_path::remove_user_path), which removes Sessions' component and
  ; leaves every other one byte-identical. The binary is still present at this
  ; point in the uninstall.
  DetailPrint "Removing Sessions per-user integration..."
  ; MAINBINARYNAME comes from Tauri's installer template. Fall back to the
  ; product name rather than failing the whole uninstall on a template detail:
  ; the safety net below still runs either way.
!ifdef MAINBINARYNAME
  nsExec::ExecToLog '"$INSTDIR\${MAINBINARYNAME}.exe" --remove-integration'
!else
  nsExec::ExecToLog '"$INSTDIR\Sessions.exe" --remove-integration'
!endif
  Pop $0

  ; Safety net, and the reason it is unconditional: if the viewer could not run
  ; at all — missing, blocked, or already deleted — the one thing that must not
  ; survive an uninstall is the entry that starts Sessions at every sign-in.
  ; Deleting an absent value is a no-op, so this is safe after a successful run.
  DeleteRegValue HKCU "Software\Microsoft\Windows\CurrentVersion\Run" "Somewhere Sessions"
!macroend
