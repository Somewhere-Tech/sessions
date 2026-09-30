buildscript {
    repositories { google(); mavenCentral() }
    dependencies {
        classpath("com.android.tools.build:gradle:8.11.0")
        classpath("org.jetbrains.kotlin:kotlin-gradle-plugin:1.9.25")
    }
}
apply(plugin = "com.android.application")
apply(plugin = "org.jetbrains.kotlin.android")
repositories { google(); mavenCentral() }

configure<com.android.build.gradle.AppExtension> {
    compileSdkVersion(36)
    namespace = "tech.somewhere.sessions.vault.fixture"
    defaultConfig {
        applicationId = "tech.somewhere.sessions.vault.fixture"
        minSdk = 24
        targetSdk = 36
        versionCode = 1
        versionName = "1"
    }
    sourceSets.getByName("main").java.apply {
        srcDirs("src/main/java", "../app/src/main/java")
        include("**/VaultInstrumentation.kt", "**/AndroidMachineVault.kt",
            "**/MachineVaultCipher.kt", "**/MachineVaultTransaction.kt")
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_1_8
        targetCompatibility = JavaVersion.VERSION_1_8
    }
}

tasks.withType<org.jetbrains.kotlin.gradle.tasks.KotlinCompile>().configureEach {
    kotlinOptions.jvmTarget = "1.8"
    exclude { it.file.name !in setOf("VaultInstrumentation.kt", "AndroidMachineVault.kt",
        "MachineVaultCipher.kt", "MachineVaultTransaction.kt") }
    setSource(files(
        "src/main/java/tech/somewhere/sessions/VaultInstrumentation.kt",
        "../app/src/main/java/tech/somewhere/sessions/AndroidMachineVault.kt",
        "../app/src/main/java/tech/somewhere/sessions/MachineVaultCipher.kt",
        "../app/src/main/java/tech/somewhere/sessions/MachineVaultTransaction.kt",
    ))
}
