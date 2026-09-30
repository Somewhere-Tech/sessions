package tech.somewhere.sessions

internal class MachineVaultException(message: String) : Exception(message)

internal interface MachineVaultStorage {
  fun read(): String?
  fun write(vault: String)
  fun remove()
}

// One plugin executor serializes transactions. A read failure is never "empty".
internal class MachineVaultTransaction(
  private val storage: MachineVaultStorage,
  private val validate: (String) -> Unit
) {
  fun load(): String? = storage.read()?.also(validate)

  fun save(vault: String): String {
    validate(vault)
    val previous = load() // Refuse to overwrite an unreadable/newer store.
    try {
      storage.write(vault)
      val confirmed = load()
      if (confirmed != vault) throw MachineVaultException("Protected credentials did not verify.")
      return confirmed
    } catch (_: Exception) {
      try {
        if (previous == null) storage.remove() else storage.write(previous)
        if (load() != previous) throw MachineVaultException("Rollback did not verify.")
      } catch (_: Exception) {
        throw MachineVaultException("Sessions could not verify or restore the protected credentials. Reopen the app before changing saved machines; do not clear app data.")
      }
      throw MachineVaultException("Sessions could not save the protected credentials; the previous set was restored. Unlock this device and try again.")
    }
  }
}
