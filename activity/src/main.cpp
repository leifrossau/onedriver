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
#include <QTimer>

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
        // --at=X,Y: open next to this point (the tray icon) instead of in the corner
        QStringList at;
        for (const QString &arg : app.arguments()) {
            if (arg.startsWith(QLatin1String("--at="))) {
                at = arg.mid(5).split(QLatin1Char(','));
            }
        }
        bool okX = false, okY = false;
        const int x = at.value(0).toInt(&okX);
        const int y = at.value(1).toInt(&okY);
        if (okX && okY) {
            popup.ToggleAt(x, y);
        } else {
            popup.Toggle();
        }
    }

    // Opt-in, for documentation screenshots: ONEDRIVER_ACTIVITY_SCREENSHOT=file.png
    // shows the window, saves it to that file after ~1.5 s and quits.
    const QString shot = qEnvironmentVariable("ONEDRIVER_ACTIVITY_SCREENSHOT");
    if (!shot.isEmpty()) {
        popup.Toggle();
        QTimer::singleShot(1500, &app, [window, shot] {
            const bool ok = window->grabWindow().save(shot);
            QCoreApplication::exit(ok ? 0 : 1);
        });
    }
    return app.exec();
}
