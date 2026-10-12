#include "backend.h"
#include "dbustypes.h"
#include "popup.h"

#include <QDBusConnection>
#include <QDBusConnectionInterface>
#include <QDBusMessage>
#include <QGuiApplication>
#include <QQmlApplicationEngine>
#include <QQuickStyle>
#include <QQuickWindow>

int main(int argc, char *argv[])
{
    QGuiApplication app(argc, argv);
    app.setApplicationName(QStringLiteral("onedriver-activity"));
    app.setDesktopFileName(QStringLiteral("onedriver-activity"));
    app.setQuitOnLastWindowClosed(false); // the process stays alive while the window is hidden

    QDBusConnection bus = QDBusConnection::sessionBus();
    if (!bus.isConnected()) {
        qCritical("onedriver-activity: cannot connect to the session bus");
        return 1;
    }

    // Single instance: if the name is taken, ask the owner to toggle and leave.
    if (!bus.registerService(QString::fromLatin1(Popup::ServiceName))) {
        QDBusMessage m = QDBusMessage::createMethodCall(QString::fromLatin1(Popup::ServiceName), QString::fromLatin1(Popup::ObjectPath),
                                                        QStringLiteral("io.github.onedriver.Activity1"), QStringLiteral("Toggle"));
        const QDBusMessage reply = bus.call(m, QDBus::Block, 3000);
        return reply.type() == QDBusMessage::ErrorMessage ? 1 : 0;
    }

    // Follow the Plasma look when the KDE style is installed.
    // (Qt falls back to "Basic" with a warning if it is missing.)
    if (qEnvironmentVariableIsEmpty("QT_QUICK_CONTROLS_STYLE")) {
        QQuickStyle::setStyle(QStringLiteral("org.kde.desktop"));
        QQuickStyle::setFallbackStyle(QStringLiteral("Basic"));
    }

    registerDBusTypes();
    Backend backend;

    QQmlApplicationEngine engine;
    engine.setInitialProperties({{QStringLiteral("backend"), QVariant::fromValue(&backend)}});
    engine.loadFromModule("org.onedriver.Activity", "Main");
    if (engine.rootObjects().isEmpty()) {
        return 1;
    }
    auto *window = qobject_cast<QQuickWindow *>(engine.rootObjects().first());
    if (!window) {
        return 1;
    }

    Popup popup(window, &backend);
    bus.registerObject(QString::fromLatin1(Popup::ObjectPath), &popup, QDBusConnection::ExportScriptableSlots);

    if (app.arguments().contains(QStringLiteral("--show"))) {
        popup.Toggle();
    }
    return app.exec();
}
