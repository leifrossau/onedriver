pragma ComponentBehavior: Bound

import QtQuick
import QtQuick.Controls as QQC2
import QtQuick.Layouts
import QtQuick.Window
import org.kde.kirigami as Kirigami

// The activity popup. Visibility, position and focus handling live in popup.cpp.
Window {
    id: win

    required property var backend

    width: 380
    height: 520
    visible: false
    color: "transparent"
    title: qsTr("OneDrive activity")

    Shortcut {
        sequence: "Escape"
        onActivated: win.hide()
    }

    Rectangle {
        id: panel
        anchors.fill: parent
        radius: 8
        color: Kirigami.Theme.backgroundColor
        border.color: Qt.alpha(Kirigami.Theme.textColor, 0.25)
        border.width: 1

        Kirigami.Theme.colorSet: Kirigami.Theme.Window
        Kirigami.Theme.inherit: false

        ColumnLayout {
            anchors.fill: parent
            anchors.margins: 1
            spacing: 0

            // ---- header ----
            ColumnLayout {
                Layout.fillWidth: true
                Layout.margins: Kirigami.Units.largeSpacing
                spacing: Kirigami.Units.smallSpacing

                QQC2.ComboBox {
                    Layout.fillWidth: true
                    visible: win.backend.instanceLabels.length > 1
                    model: win.backend.instanceLabels
                    currentIndex: win.backend.currentIndex
                    onActivated: index => win.backend.currentIndex = index
                }

                RowLayout {
                    Layout.fillWidth: true
                    spacing: Kirigami.Units.largeSpacing

                    Kirigami.Icon {
                        source: win.backend.stateIcon
                        implicitWidth: Kirigami.Units.iconSizes.medium
                        implicitHeight: Kirigami.Units.iconSizes.medium
                    }
                    ColumnLayout {
                        Layout.fillWidth: true
                        spacing: 0
                        QQC2.Label {
                            Layout.fillWidth: true
                            text: win.backend.stateText
                            font.bold: true
                            elide: Text.ElideRight
                            color: win.backend.isError ? Kirigami.Theme.negativeTextColor : Kirigami.Theme.textColor
                        }
                        QQC2.Label {
                            Layout.fillWidth: true
                            visible: text !== ""
                            text: win.backend.account
                            elide: Text.ElideRight
                            opacity: 0.7
                            font: Kirigami.Theme.smallFont
                        }
                    }
                }

                ColumnLayout {
                    Layout.fillWidth: true
                    visible: win.backend.quotaText !== ""
                    spacing: 2
                    QQC2.ProgressBar {
                        Layout.fillWidth: true
                        from: 0
                        to: 1
                        value: win.backend.quotaFraction
                    }
                    QQC2.Label {
                        text: win.backend.quotaText
                        opacity: 0.7
                        font: Kirigami.Theme.smallFont
                    }
                }
            }

            Kirigami.Separator { Layout.fillWidth: true }

            // ---- lists ----
            QQC2.ScrollView {
                id: scroll
                Layout.fillWidth: true
                Layout.fillHeight: true
                contentWidth: availableWidth
                clip: true

                ColumnLayout {
                    width: scroll.availableWidth
                    spacing: 0

                    // -- syncing now --
                    SectionHeader {
                        visible: win.backend.transfersSupported && win.backend.connected
                        text: qsTr("Syncing now")
                    }

                    ColumnLayout {
                        Layout.fillWidth: true
                        Layout.topMargin: Kirigami.Units.largeSpacing
                        Layout.bottomMargin: Kirigami.Units.largeSpacing
                        visible: win.backend.transfersSupported && win.backend.connected && transfers.count === 0
                        spacing: Kirigami.Units.smallSpacing

                        Kirigami.Icon {
                            Layout.alignment: Qt.AlignHCenter
                            source: "emblem-ok-symbolic"
                            implicitWidth: Kirigami.Units.iconSizes.large
                            implicitHeight: Kirigami.Units.iconSizes.large
                        }
                        QQC2.Label {
                            Layout.alignment: Qt.AlignHCenter
                            text: qsTr("Everything is up to date")
                            opacity: 0.8
                        }
                    }

                    Repeater {
                        id: transfers
                        model: win.backend.transfersSupported ? win.backend.transfers : null

                        delegate: QQC2.ItemDelegate {
                            id: tRow
                            required property string name
                            required property string folder
                            required property string path
                            required property string direction
                            required property string xferState
                            required property string stateText
                            required property real fraction
                            required property string progressText
                            required property string iconName

                            Layout.fillWidth: true
                            onClicked: win.backend.openFolder(tRow.path)

                            contentItem: RowLayout {
                                spacing: Kirigami.Units.largeSpacing
                                Kirigami.Icon {
                                    source: tRow.iconName
                                    implicitWidth: Kirigami.Units.iconSizes.medium
                                    implicitHeight: Kirigami.Units.iconSizes.medium
                                }
                                ColumnLayout {
                                    Layout.fillWidth: true
                                    spacing: 1
                                    QQC2.Label {
                                        Layout.fillWidth: true
                                        text: tRow.name
                                        elide: Text.ElideMiddle
                                    }
                                    QQC2.Label {
                                        Layout.fillWidth: true
                                        text: tRow.folder !== "" ? tRow.folder : qsTr("OneDrive")
                                        elide: Text.ElideLeft
                                        opacity: 0.7
                                        font: Kirigami.Theme.smallFont
                                    }
                                    QQC2.ProgressBar {
                                        Layout.fillWidth: true
                                        visible: tRow.xferState === "active"
                                        indeterminate: tRow.fraction < 0
                                        from: 0
                                        to: 1
                                        value: Math.max(0, tRow.fraction)
                                    }
                                    QQC2.Label {
                                        Layout.fillWidth: true
                                        text: tRow.xferState === "active"
                                              ? tRow.progressText
                                              : (tRow.progressText !== "" ? tRow.stateText + " · " + tRow.progressText : tRow.stateText)
                                        elide: Text.ElideRight
                                        opacity: 0.7
                                        font: Kirigami.Theme.smallFont
                                    }
                                }
                                Kirigami.Icon {
                                    source: tRow.direction === "upload" ? "go-up" : "go-down"
                                    implicitWidth: Kirigami.Units.iconSizes.small
                                    implicitHeight: Kirigami.Units.iconSizes.small
                                    opacity: 0.8
                                }
                            }
                        }
                    }

                    // -- recent --
                    SectionHeader {
                        visible: win.backend.connected
                        text: qsTr("Recent")
                    }

                    QQC2.Label {
                        Layout.fillWidth: true
                        Layout.margins: Kirigami.Units.largeSpacing
                        visible: win.backend.connected && recent.count === 0
                        text: qsTr("No recent activity")
                        opacity: 0.7
                    }

                    Repeater {
                        id: recent
                        model: win.backend.recent

                        delegate: QQC2.ItemDelegate {
                            id: rRow
                            required property string name
                            required property string folder
                            required property string path
                            required property string syncAction
                            required property string actionText
                            required property string actionIcon
                            required property string timeText
                            required property string iconName

                            Layout.fillWidth: true
                            onClicked: win.backend.openFile(rRow.path)

                            TapHandler {
                                acceptedButtons: Qt.RightButton
                                onTapped: menu.popup()
                            }

                            QQC2.Menu {
                                id: menu
                                QQC2.MenuItem {
                                    text: qsTr("Open folder")
                                    icon.name: "folder-open"
                                    onTriggered: win.backend.openFolder(rRow.path)
                                }
                                QQC2.MenuItem {
                                    text: qsTr("View online")
                                    icon.name: "internet-services"
                                    enabled: rRow.syncAction !== "deleted"
                                    onTriggered: win.backend.viewOnline(rRow.path)
                                }
                            }

                            contentItem: RowLayout {
                                spacing: Kirigami.Units.largeSpacing
                                Kirigami.Icon {
                                    source: rRow.iconName
                                    implicitWidth: Kirigami.Units.iconSizes.medium
                                    implicitHeight: Kirigami.Units.iconSizes.medium
                                }
                                ColumnLayout {
                                    Layout.fillWidth: true
                                    spacing: 1
                                    QQC2.Label {
                                        Layout.fillWidth: true
                                        text: rRow.name
                                        elide: Text.ElideMiddle
                                    }
                                    RowLayout {
                                        Layout.fillWidth: true
                                        spacing: Kirigami.Units.smallSpacing
                                        Kirigami.Icon {
                                            source: rRow.actionIcon
                                            implicitWidth: Kirigami.Units.iconSizes.small
                                            implicitHeight: Kirigami.Units.iconSizes.small
                                            color: rRow.syncAction === "error" ? Kirigami.Theme.negativeTextColor
                                                 : rRow.syncAction === "conflict" ? Kirigami.Theme.neutralTextColor
                                                 : Kirigami.Theme.textColor
                                        }
                                        QQC2.Label {
                                            Layout.fillWidth: true
                                            text: rRow.actionText + " · " + rRow.timeText
                                            elide: Text.ElideRight
                                            opacity: 0.7
                                            font: Kirigami.Theme.smallFont
                                        }
                                    }
                                }
                            }
                        }
                    }
                }
            }

            Kirigami.Separator { Layout.fillWidth: true }

            // ---- footer ----
            RowLayout {
                Layout.fillWidth: true
                Layout.margins: Kirigami.Units.smallSpacing
                spacing: 0

                QQC2.ToolButton {
                    Layout.fillWidth: true
                    text: qsTr("Open folder")
                    icon.name: "folder-open"
                    enabled: win.backend.connected
                    onClicked: win.backend.openMountPoint()
                }
                QQC2.ToolButton {
                    Layout.fillWidth: true
                    text: win.backend.paused ? qsTr("Resume") : qsTr("Pause")
                    icon.name: win.backend.paused ? "media-playback-start" : "media-playback-pause"
                    enabled: win.backend.connected
                    onClicked: win.backend.togglePause()
                }
                QQC2.ToolButton {
                    Layout.fillWidth: true
                    text: qsTr("Sync now")
                    icon.name: "view-refresh"
                    enabled: win.backend.connected
                    onClicked: win.backend.syncNow()
                }
                QQC2.ToolButton {
                    Layout.fillWidth: true
                    text: qsTr("Settings")
                    icon.name: "configure"
                    onClicked: {
                        win.backend.openSettings()
                        win.hide()
                    }
                }
            }
        }
    }

    component SectionHeader: QQC2.Label {
        Layout.fillWidth: true
        Layout.leftMargin: Kirigami.Units.largeSpacing
        Layout.rightMargin: Kirigami.Units.largeSpacing
        Layout.topMargin: Kirigami.Units.largeSpacing
        font.bold: true
        opacity: 0.8
    }
}
